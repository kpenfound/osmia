package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kpenfound/osmia/internal/servicetest/lifecyclefixture"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesignProductionLifecyclePreservesPendingProjectJournal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, gate := newLifecycleDesignFixture(t)
	defer f.Stop(t)
	configFile, err := os.OpenFile(filepath.Join(f.S.Current().Root.String(), "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString("[roles.mason]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
	fixturekit.Must(t, err)
	fixturekit.Must(t, configFile.Close())
	f.Stop(t)
	f.Start(t)
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err = f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	origin, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	defer func() {
		if t.Failed() {
			t.Logf("cleanup checkpoint clock=%v", f.Clock.Now())
			lifecyclefixture.LogUnsettled(t, origin, f.Stream1, journey.DeltaRunner{S: f.S}.Load)
		}
	}()
	authority, host, _, closeAuthority, err := f.S.Lifecycle().ManagerRepository(ctx, origin, f.Stream1)
	fixturekit.Must(t, err)
	approval, pins, found, err := trace.ReadGroupApproval(authority, host)
	closeAuthority()
	fixturekit.Must(t, err)
	if !found || !approval.Complete {
		t.Fatal("production group did not reach effective seals")
	}
	var citation string
	for _, slice := range approval.ProjectSpec.Slices {
		if slice.Workstream == f.Stream1 && len(slice.ContractCriteria) > 0 {
			citation = fmt.Sprintf("spec#%d", slice.ContractCriteria[0])
		}
	}
	if citation == "" {
		t.Fatal("production slice has no protected contract criterion")
	}
	var routed atomic.Bool
	f.Engine.Mu.Lock()
	f.Engine.Turns["lifecycle-project-journal"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "May the cross-repo acknowledgment contract change?"})
		return intakefixture.QuestionResult(req, "session-journal-worker", "Asked the original shared manager."), err
	}
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil || !slices.Contains(names, status.ToolName) {
			return intakefixture.QuestionResult(req, "session-journal-worker", "Waiting for the project decision."), err
		}
		if !routed.Load() {
			qs, err := origin.Questions(f.Stream1)
			if err != nil {
				return nil, err
			}
			for _, q := range qs {
				if q.State != trace.QuestionOpen {
					continue
				}
				result, err := fixturekit.CallTool(ctx, tools, questions.RouteAmendmentTool, map[string]any{"question": string(f.Stream1) + ":" + q.Asked.ID, "citations": []string{citation}, "change": "Revise the shared acknowledgment contract.", "reason": "The original member raised a contract gap."})
				if err != nil {
					return nil, err
				}
				if !strings.Contains(result, `"recorded":true`) || !strings.Contains(result, `"project_amendment"`) {
					return nil, fmt.Errorf("project routing refused: %s", result)
				}
				routed.Store(true)
			}
		}
		_, err = fixturekit.CallTool(ctx, tools, status.ToolName, map[string]any{"goal": "Preserve the pending project decision.", "note": "The contract request remains pending.", "agents": []string{}})
		return intakefixture.QuestionResult(req, "session-journal-manager", "The pending request remains in the original authority."), err
	}
	f.Engine.Mu.Unlock()
	journey.LifecycleAwait(t, f, "production member admission", func() bool {
		state, err := origin.Workflow(f.Stream1, trace.UnitSubject("accept"))
		fixturekit.Must(t, err)
		if state.Value != intake.UnitImplementing {
			return false
		}
		_, err = origin.Thread(f.Stream1, intake.MasonAgent("accept"))
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		fixturekit.Must(t, err)
		return true
	})
	worker, err := origin.Thread(f.Stream1, intake.MasonAgent("accept"))
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_lifecycle_project_journal", Project: f.Project, Workstream: f.Stream1, Unit: "accept", At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "workstream_created"}
	_, err = origin.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: worker.Identity.ID, ThreadID: worker.Identity.ThreadID, TurnID: "lifecycle-project-journal", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the admitted member mason.", Prompt: "May the cross-repo acknowledgment contract change?"})
	fixturekit.Must(t, err)
	journey.LifecycleAwait(t, f, "production contract routing", func() bool {
		qs, err := origin.Questions(f.Stream1)
		fixturekit.Must(t, err)
		return routed.Load() && len(qs) == 1 && qs[0].State == trace.QuestionRouted
	})
	authority, err = trace.OpenWorkgroupAuthority(f.S.Current().Root, approval.Workgroup)
	fixturekit.Must(t, err)
	pending, revision, err := trace.ProjectAmendments(authority)
	closeErr := authority.Close()
	fixturekit.Must(t, err)
	fixturekit.Must(t, closeErr)
	if len(pending) != 1 || revision != 1 || pending[0].Origin != f.Stream1 || pending[0].QuestionID != "1" || !slices.Equal(pending[0].Citations, []string{citation}) {
		t.Fatalf("production did not record the original request: %+v", pending)
	}
	asking, err := origin.Questions(f.Stream1)
	fixturekit.Must(t, err)
	_, err = f.C.Abandon(ctx, f.Stream1, "Owner cancelled the requesting member.")
	fixturekit.Must(t, err)
	_, err = f.C.Archive(ctx, f.Stream1)
	fixturekit.Must(t, err)
	gate.Await(t, f)
	check := func(boundary string) {
		t.Helper()
		journal, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, approval.Workgroup)
		fixturekit.Must(t, err)
		defer journal.Close()
		got, gotRevision, err := trace.ProjectAmendments(journal)
		fixturekit.Must(t, err)
		decision, document, found, err := trace.ReadGroupApproval(journal, host)
		fixturekit.Must(t, err)
		if !intakefixture.EqualJSONTest(got, pending) || gotRevision != revision || !found || !intakefixture.EqualJSONTest(decision, approval) || !intakefixture.EqualJSONTest(document, pins) {
			t.Fatalf("%s changed pending journal or exact approval pins", boundary)
		}
		live, err := f.S.Repository(f.Project2)
		fixturekit.Must(t, err)
		target, _, _, release, err := f.S.Lifecycle().ManagerRepository(ctx, live, f.Stream2)
		fixturekit.Must(t, err)
		defer release()
		saved, err := trace.Get[trace.Document](target, host, "member-questions-"+string(f.Stream1), 1)
		fixturekit.Must(t, err)
		var snapshot struct {
			Questions []trace.QuestionState `json:"questions"`
		}
		fixturekit.Must(t, json.Unmarshal([]byte(saved.Content), &snapshot))
		if !intakefixture.EqualJSONTest(snapshot.Questions, asking) {
			t.Fatalf("%s changed original routed asking links", boundary)
		}
		if _, err := target.Workflow(f.Stream1, trace.FeatureSubject); err == nil || !strings.Contains(err.Error(), trace.ErrMemberRetired.Error()) {
			t.Fatalf("%s unavailable origin did not retain a recoverable hold: %v", boundary, err)
		}
		sent, err := f.C.Send(ctx, f.Stream2, "Report the pending project request at "+boundary)
		fixturekit.Must(t, err)
		journey.LifecycleAwait(t, f, "pending journal owner response "+boundary, func() bool {
			conversation, err := f.C.Conversation(ctx, f.Stream2)
			fixturekit.Must(t, err)
			return slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool {
				return e.Turn == sent.Turn && e.Kind == "response" && e.State == api.TurnDone
			})
		})
		got, gotRevision, err = trace.ProjectAmendments(journal)
		fixturekit.Must(t, err)
		if !intakefixture.EqualJSONTest(got, pending) || gotRevision != revision {
			t.Fatal("group access changed or discarded an unapplied request")
		}
	}
	check("retained")
	gate.Release()
	journey.LifecycleAwait(t, f, "pending-origin cleanup", func() bool {
		a, found := intakefixture.ArchiveRecord(t, f.S.Store, f.Stream1)
		return found && !a.CleanedAt.IsZero()
	})
	check("deleted")
	f.Stop(t)
	f.Start(t)
	check("restarted")
	gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, f.Project, f.Stream1)
	fixturekit.Must(t, err)
	if !gone {
		t.Fatal("journal recovery recreated retired history")
	}
}
