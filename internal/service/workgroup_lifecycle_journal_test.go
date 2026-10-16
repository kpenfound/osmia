package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesignProductionLifecyclePreservesPendingProjectJournal(t *testing.T) {
	ctx := context.Background()
	f, gate := newLifecycleDesignFixture(t)
	defer f.stop(t)
	// The deterministic engine runs admitted masons through the same
	// container boundary as the fixture's chief and committee.
	configFile, err := os.OpenFile(filepath.Join(f.s.current().Root.String(), "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	must(t, err)
	_, err = configFile.WriteString("[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
	must(t, err)
	must(t, configFile.Close())
	f.stop(t)
	f.start(t)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err = f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
	must(t, err)
	f.awaitApproved(t)
	origin, err := f.s.repository(f.project)
	must(t, err)
	defer func() {
		if t.Failed() {
			t.Logf("cleanup checkpoint clock=%v", f.clock.Now())
			logUnsettledLifecycleOperations(t, origin, f.stream1, f.s)
		}
	}()
	authority, host, _, closeAuthority, err := f.s.chiefRepository(ctx, origin, f.stream1)
	must(t, err)
	approval, pins, found, err := trace.ReadGroupApproval(authority, host)
	closeAuthority()
	must(t, err)
	if !found || !approval.Complete {
		t.Fatal("production group did not reach effective seals")
	}
	var citation string
	for _, slice := range approval.ProjectSpec.Slices {
		if slice.Workstream == f.stream1 && len(slice.ContractCriteria) > 0 {
			citation = fmt.Sprintf("spec#%d", slice.ContractCriteria[0])
		}
	}
	if citation == "" {
		t.Fatal("production slice has no protected contract criterion")
	}
	var routed atomic.Bool
	f.engine.mu.Lock()
	f.engine.turns["lifecycle-project-journal"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "May the cross-repo acknowledgment contract change?"})
		return questionResult(req, "session-journal-worker", "Asked the original shared chief."), err
	}
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := toolNames(ctx, tools)
		if err != nil || !slices.Contains(names, status.ToolName) {
			return questionResult(req, "session-journal-worker", "Waiting for the project decision."), err
		}
		if !routed.Load() {
			qs, err := origin.Questions(f.stream1)
			if err != nil {
				return nil, err
			}
			for _, q := range qs {
				if q.State != trace.QuestionOpen {
					continue
				}
				result, err := callTool(ctx, tools, questions.RouteAmendmentTool, map[string]any{"question": string(f.stream1) + ":" + q.Asked.ID, "citations": []string{citation}, "change": "Revise the shared acknowledgment contract.", "reason": "The original member raised a contract gap."})
				if err != nil {
					return nil, err
				}
				if !strings.Contains(result, `"recorded":true`) || !strings.Contains(result, `"project_amendment"`) {
					return nil, fmt.Errorf("project routing refused: %s", result)
				}
				routed.Store(true)
			}
		}
		_, err = callTool(ctx, tools, status.ToolName, map[string]any{"goal": "Preserve the pending project decision.", "note": "The contract request remains pending.", "agents": []string{}})
		return questionResult(req, "session-journal-chief", "The pending request remains in the original authority."), err
	}
	f.engine.mu.Unlock()
	// Let the live builder and mason controller admit the real sealed unit.
	lifecycleAwait(t, f, "production member admission", func() bool {
		state, err := origin.Workflow(f.stream1, trace.UnitSubject("accept"))
		must(t, err)
		if state.Value != UnitImplementing {
			return false
		}
		_, err = origin.Thread(f.stream1, masonAgent("accept"))
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		must(t, err)
		return true
	})
	worker, err := origin.Thread(f.stream1, masonAgent("accept"))
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_lifecycle_project_journal", Project: f.project, Workstream: f.stream1, Unit: "accept", At: f.clock.Now(), Actor: ownerActor, Cause: "workstream_created"}
	_, err = origin.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: worker.Identity.ID, ThreadID: worker.Identity.ThreadID, TurnID: "lifecycle-project-journal", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the admitted member mason.", Prompt: "May the cross-repo acknowledgment contract change?"})
	must(t, err)
	lifecycleAwait(t, f, "production contract routing", func() bool {
		qs, err := origin.Questions(f.stream1)
		must(t, err)
		return routed.Load() && len(qs) == 1 && qs[0].State == trace.QuestionRouted
	})
	authority, err = trace.OpenWorkgroupAuthority(f.s.current().Root, approval.Workgroup)
	must(t, err)
	pending, revision, err := trace.ProjectAmendments(authority)
	closeErr := authority.Close()
	must(t, err)
	must(t, closeErr)
	if len(pending) != 1 || revision != 1 || pending[0].Origin != f.stream1 || pending[0].QuestionID != "1" || !slices.Equal(pending[0].Citations, []string{citation}) {
		t.Fatalf("production did not record the original request: %+v", pending)
	}
	asking, err := origin.Questions(f.stream1)
	must(t, err)
	_, err = f.c.Abandon(ctx, f.stream1, "Owner cancelled the requesting member.")
	must(t, err)
	_, err = f.c.Archive(ctx, f.stream1)
	must(t, err)
	gate.await(t)
	check := func(boundary string) {
		t.Helper()
		journal, err := trace.OpenWorkgroupAuthority(f.s.current().Root, approval.Workgroup)
		must(t, err)
		defer journal.Close()
		got, gotRevision, err := trace.ProjectAmendments(journal)
		must(t, err)
		decision, document, found, err := trace.ReadGroupApproval(journal, host)
		must(t, err)
		if !equalJSONTest(got, pending) || gotRevision != revision || !found || !equalJSONTest(decision, approval) || !equalJSONTest(document, pins) {
			t.Fatalf("%s changed pending journal or exact approval pins", boundary)
		}
		live, err := f.s.repository(f.project2)
		must(t, err)
		target, _, _, release, err := f.s.chiefRepository(ctx, live, f.stream2)
		must(t, err)
		defer release()
		saved, err := trace.Get[trace.Document](target, host, "member-questions-"+string(f.stream1), 1)
		must(t, err)
		var snapshot struct {
			Questions []trace.QuestionState `json:"questions"`
		}
		must(t, json.Unmarshal([]byte(saved.Content), &snapshot))
		if !equalJSONTest(snapshot.Questions, asking) {
			t.Fatalf("%s changed original routed asking links", boundary)
		}
		// Reading the journal grants no remaining member-specific application.
		if _, err := target.Workflow(f.stream1, trace.FeatureSubject); err == nil || !strings.Contains(err.Error(), trace.ErrMemberRetired.Error()) {
			t.Fatalf("%s unavailable origin did not retain a recoverable hold: %v", boundary, err)
		}
		sent, err := f.c.Send(ctx, f.stream2, "Report the pending project request at "+boundary)
		must(t, err)
		lifecycleAwait(t, f, "pending journal owner response "+boundary, func() bool {
			conversation, err := f.c.Conversation(ctx, f.stream2)
			must(t, err)
			return slices.ContainsFunc(conversation.Entries, func(e ConversationEntry) bool {
				return e.Turn == sent.Turn && e.Kind == "response" && e.State == TurnDone
			})
		})
		got, gotRevision, err = trace.ProjectAmendments(journal)
		must(t, err)
		if !equalJSONTest(got, pending) || gotRevision != revision {
			t.Fatal("group access changed or discarded an unapplied request")
		}
	}
	check("retained")
	gate.release()
	lifecycleAwait(t, f, "pending-origin cleanup", func() bool {
		a, found := archiveRecord(t, f.s.store, f.stream1)
		return found && !a.CleanedAt.IsZero()
	})
	check("deleted")
	f.stop(t)
	f.start(t)
	check("restarted")
	gone, err := trace.WorkstreamTraceGone(f.s.current().Root, f.project, f.stream1)
	must(t, err)
	if !gone {
		t.Fatal("journal recovery recreated retired history")
	}
}
