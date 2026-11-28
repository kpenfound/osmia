package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

const savedManagerIdentity = `{"schema":"osmia.trace.agent","version":1,"id":"chief_of_staff","revision":1,"project":%q,"workstream":%q,"at":"2026-09-15T12:00:00Z","actor":{"kind":"owner","id":"local"},"cause":"chief-of-staff-create","depth":0,"role":"chief_of_staff","thread_id":"chief_of_staff","session":{"Backend":"","ID":""}}`

const savedManagerThread = `{"identity":%s,"session":{"Backend":"","ID":""},"status":"idle"}`

const savedManagerIdentityPath = "workstreams/%s/agents/chief_of_staff/identity.jsonl"

func TestTheStoredNameOfTheManagerIsTheNameSavedTracesHold(t *testing.T) {
	if Manager != "chief_of_staff" || StatusRole != "chief_of_staff" || DecidedByManager != "chief_of_staff" {
		t.Fatalf("stored manager names: %q %q %q", Manager, StatusRole, DecidedByManager)
	}
	if ManagerKey != "chief" {
		t.Fatalf("stored manager key: %q", ManagerKey)
	}
	if !ValidDecider(DecidedByManager, "") || !ValidDecider("chief_of_staff", "") {
		t.Fatal("the manager's stored decider is refused")
	}
}

func TestANewManagerThreadIsStoredExactlyAsTracesSavedBeforeStoredIt(t *testing.T) {
	r, _, _ := create(t)
	s := mustScoped(t, r, streamID)
	identity := fmt.Sprintf(savedManagerIdentity, projectID, streamID)
	if got := storedBodies(t, s, fmt.Sprintf(savedManagerIdentityPath, streamID)); len(got) != 1 || got[0] != identity {
		t.Fatalf("stored manager identity = %q, want %q", got, identity)
	}
	var thread string
	if err := s.db.QueryRowContext(context.Background(), "SELECT body FROM workflow_threads WHERE agent_id = 'chief_of_staff'").Scan(&thread); err != nil || thread != fmt.Sprintf(savedManagerThread, identity) {
		t.Fatalf("stored manager thread = %q, %v", thread, err)
	}
}

func TestTheManagerThreadOfATraceSavedBeforeTheChangeResumesForANewTurn(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	threadlessWorkstream(t, r, threadlessStream)
	s := mustScoped(t, r, threadlessStream)
	identity := fmt.Sprintf(savedManagerIdentity, projectID, threadlessStream)
	storeExec(t, s, "INSERT INTO records (commit_seq, path, schema, id, revision, body) VALUES ((SELECT max(seq) FROM commits), ?, 'osmia.trace.agent', 'chief_of_staff', 1, ?)", fmt.Sprintf(savedManagerIdentityPath, threadlessStream), identity)
	storeExec(t, s, "INSERT INTO workflow_threads (agent_id, body) VALUES ('chief_of_staff', ?)", fmt.Sprintf(savedManagerThread, identity))
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	saved, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer saved.Close()

	thread, err := saved.ManagerThread(threadlessStream)
	if err != nil {
		t.Fatal(err)
	}
	if id := thread.Identity; id.ID != "chief_of_staff" || id.Role != "chief_of_staff" || id.ThreadID != "chief_of_staff" || thread.Status != "idle" || len(thread.Turns) != 0 {
		t.Fatalf("saved manager thread = %+v", thread)
	}
	ensured, err := saved.EnsureManager(ctx, threadlessStream, at.Add(time.Hour), Actor{Kind: "service", ID: "osmia"})
	if err != nil || !equalJSON(ensured.Identity, thread.Identity) {
		t.Fatalf("ensure over the saved identity = %+v, %v", ensured, err)
	}

	header := Header{Schema: "osmia.trace.turn-request", Version: Version, ID: "request_resume", Revision: 1, Project: projectID, Workstream: threadlessStream, At: at, Actor: owner, Cause: "owner-message-1"}
	request := TurnRequest{Header: header, AgentID: "chief_of_staff", ThreadID: "chief_of_staff", TurnID: "resume", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Carry on"}
	if _, err := saved.EnqueueTurn(ctx, request); err != nil {
		t.Fatal(err)
	}
	queued, err := saved.ClaimTurn(ctx, threadlessStream, Manager, "claim", "/owned/resume", at)
	if err != nil {
		t.Fatal(err)
	}
	response := header
	response.Schema, response.ID, response.At = "osmia.trace.turn-response", "response_resume", at.Add(time.Second)
	result := coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "continued"}, SessionDirectory: queued.Claim.SessionDirectory, StartedAt: at, Duration: time.Second, FinalResponse: "Resumed"}
	if err := saved.CaptureTurn(ctx, "claim", TurnResponse{Header: response, AgentID: "chief_of_staff", ThreadID: "chief_of_staff", TurnID: "resume", RequestID: request.ID, RequestRevision: 1, Result: result}); err != nil {
		t.Fatal(err)
	}
	if err := saved.CompleteTurn(ctx, threadlessStream, Manager, "resume", "claim", at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	again, err := saved.ManagerThread(threadlessStream)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Turns) != 1 || again.Turns[0].Request.TurnID != "resume" || again.Turns[0].Response == nil || again.Session.ID != "continued" {
		t.Fatalf("manager thread after the new turn = %+v", again)
	}
	for _, path := range []string{fmt.Sprintf("workstreams/%s/agents/chief_of_staff/log.jsonl", threadlessStream)} {
		bodies := storedBodies(t, mustScoped(t, saved, threadlessStream), path)
		if len(bodies) != 2 || !strings.Contains(bodies[0], `"agent_id":"chief_of_staff"`) || !strings.Contains(bodies[1], `"thread_id":"chief_of_staff"`) {
			t.Fatalf("records under %s = %q", path, bodies)
		}
	}
	var agents []string
	rows, err := mustScoped(t, saved, threadlessStream).db.QueryContext(ctx, "SELECT agent_id FROM workflow_threads")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		agents = append(agents, id)
	}
	if b, _ := json.Marshal(agents); string(b) != `["chief_of_staff"]` {
		t.Fatalf("threads = %s", b)
	}
}
