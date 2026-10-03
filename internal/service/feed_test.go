package service

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// The feed merges the conversation with the chief of staff, its statuses,
// the sessions that started and the workstream's and units' state changes in
// time order. A turn answering an owner message shows as that message, not
// as a session; a queued turn that never started and a change of anything
// but the workstream or a unit stay out.
func TestFeedOrdersEverythingThatHappened(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, cfg := conversationFixture(t, "fd-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	at := func(minutes int) time.Time { return demoStart.Add(time.Duration(minutes) * time.Minute) }
	header := func(schema, id string, minutes int) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: at(minutes), Actor: serviceActor, Cause: "test"}
	}
	transition := func(subject, unit, to string, minutes int) {
		t.Helper()
		state, err := repo.Workflow(stream, subject)
		must(t, err)
		h := header("osmia.trace.transition", subject+"-"+to, minutes)
		h.Unit = unit
		_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: to, Reason: "because " + to}})
		must(t, err)
	}
	finish := func(agent string, q trace.QueuedTurn, result coreadapter.SessionResult, minutes int) {
		t.Helper()
		h := q.Request.Header
		h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), at(minutes)
		result.Session = coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "session-" + agent}
		result.StartedAt, result.SessionDirectory = q.Claim.At, q.Claim.SessionDirectory
		must(t, repo.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: agent, ThreadID: agent, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision, Result: result}))
		must(t, repo.CompleteTurn(ctx, stream, agent, q.Request.TurnID, q.Claim.Token, at(minutes)))
	}

	transition(trace.FeatureSubject, "", BuildingState, 1)
	transition(trace.UnitSubject("upload"), "upload", UnitReady, 2)
	transition(trace.QuestionSubject("q1"), "", "asked", 3)

	// The chief of staff runs on events and writes a status during the turn.
	_, err = repo.EnsureChiefOfStaff(ctx, stream, at(0), serviceActor)
	must(t, err)
	chiefProfile := coreadapter.Profile{Name: "chief", Backend: "fake", Model: "test"}
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: header("osmia.trace.turn-request", "request_events", 4), AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "events", Profile: chiefProfile, Prompt: "Events"})
	must(t, err)
	chief, err := repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token_events", filepath.Join(t.TempDir(), "events"), at(5))
	must(t, err)
	scope := coreadapter.Scope{Project: string(project), Workstream: string(stream), Thread: trace.ChiefOfStaff, Turn: "events", Role: trace.ChiefOfStaff}
	content := trace.StatusContent{Goal: "Ship uploads.", Attention: "Rule on the API.", Note: "Upload is ready.", Agents: []string{"A mason builds upload."}}
	_, err = repo.SetStatus(ctx, trace.ChiefOfStaff, scope, content, at(6), func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	must(t, err)
	finish(trace.ChiefOfStaff, chief, coreadapter.SessionResult{FinalResponse: "Status written."}, 7)

	// A mason starts on the unit, which then moves to implementing, and
	// reports; another mason's turn is queued and never starts.
	queueRoleTurn(t, repo, stream, "agent_mason_upload", masonRole, "upload", at(8))
	mason, err := repo.ClaimTurn(ctx, stream, "agent_mason_upload", "token_upload", filepath.Join(t.TempDir(), "upload"), at(9))
	must(t, err)
	transition(trace.UnitSubject("upload"), "upload", UnitImplementing, 10)
	queueRoleTurn(t, repo, stream, "agent_mason_audit", masonRole, "audit", at(11))
	finish("agent_mason_upload", mason, coreadapter.SessionResult{FinalResponse: "Long answer", Outcome: &coreadapter.Outcome{Status: masonDone, Report: "Built upload."}}, 12)

	// The owner's message is queued behind nothing and has not run.
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_owner", Revision: 1, Project: project, Workstream: stream, At: at(13), Actor: ownerActor, Cause: "owner-message"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "message_owner", Profile: chiefProfile, Prompt: "How is it going?"})
	must(t, err)

	got, err := feedEntries(repo, stream)
	must(t, err)
	chiefEnded, masonEnded := at(7), at(12)
	want := []FeedEntry{
		{Kind: "transition", At: at(1), Transition: &FeedTransition{From: "", To: BuildingState, Actor: "service:osmia", Reason: "because " + BuildingState}},
		{Kind: "transition", At: at(2), Transition: &FeedTransition{Unit: "upload", From: "", To: UnitReady, Actor: "service:osmia", Reason: "because " + UnitReady}},
		{Kind: "session", At: at(5), Session: &FeedSession{Agent: trace.ChiefOfStaff, Role: trace.ChiefOfStaff, Turn: "events", Profile: "chief", State: "done", StartedAt: at(5), EndedAt: &chiefEnded, Summary: "Status written."}},
		{Kind: "status", At: at(6), Status: &StatusView{Goal: "Ship uploads.", Attention: "Rule on the API.", Note: "Upload is ready.", Agents: []string{"A mason builds upload."}, Revision: 1, UpdatedAt: at(6)}},
		{Kind: "session", At: at(9), Session: &FeedSession{Agent: "agent_mason_upload", Role: masonRole, Unit: "upload", Turn: "agent_mason_upload-turn", Profile: "default", State: "done", StartedAt: at(9), EndedAt: &masonEnded, Summary: "Built upload."}},
		{Kind: "transition", At: at(10), Transition: &FeedTransition{Unit: "upload", From: UnitReady, To: UnitImplementing, Actor: "service:osmia", Reason: "because " + UnitImplementing}},
		{Kind: "message", At: at(13), Turn: "message_owner", Text: "How is it going?", State: TurnQueued},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("feed:\n%s\nwant:\n%s", describeFeed(got), describeFeed(want))
	}
}

// describeFeed prints one line per entry, with the time a session ended.
func describeFeed(entries []FeedEntry) string {
	var out strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&out, "%s %s %s %s %s", e.At.Format(time.TimeOnly), e.Kind, e.Turn, e.State, e.Text)
		switch {
		case e.Session != nil:
			fmt.Fprintf(&out, " %+v", *e.Session)
			if e.Session.EndedAt != nil {
				fmt.Fprintf(&out, " ended %s", e.Session.EndedAt.Format(time.TimeOnly))
			}
		case e.Status != nil:
			fmt.Fprintf(&out, " %+v", *e.Status)
		case e.Transition != nil:
			fmt.Fprintf(&out, " %+v", *e.Transition)
		}
		out.WriteString("\n")
	}
	return out.String()
}
