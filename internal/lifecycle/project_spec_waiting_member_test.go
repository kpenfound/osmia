package lifecycle_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A sketched member that waits for the workgroup spec while another member's
// drafting stopped is told which member that is and what to do; the stopped
// member's own move to sketched starts a new run of drafts.
func TestAWaitingMemberNamesTheMemberWhoseDraftingStopped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var allowed atomic.Value
	var released atomic.Bool
	f := newCrossRepoRatifyFixture(t, func(opts *Options) {
		hold := opts.Reconciliation.Hold
		opts.Reconciliation.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
			if op.Action == DraftAction && !released.Load() {
				if s, _ := allowed.Load().(config.WorkstreamID); s != stream {
					return true
				}
			}
			return hold != nil && hold(stream, op)
		}
	})
	defer f.stop(t)
	f.engine.Mu.Lock()
	scripted := f.engine.Turns["draft-1-1"]
	f.engine.Turns["draft-1-1"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Workspace.Directory(), string(f.stream2)) {
			return nil, errors.New("the architect is unavailable")
		}
		return scripted(ctx, req, turn, tools)
	}
	f.engine.Mu.Unlock()
	if _, api := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2}); api != nil {
		t.Fatal(api)
	}
	allowed.Store(f.stream1)
	released.Store(true)
	repo1, repo2 := mustRepo(t, f.s, f.project), mustRepo(t, f.s, f.project2)
	repo1.WakeWorkflow()
	repo2.WakeWorkflow()
	eventually(t, "the second member's drafting stops", func() bool {
		state, err := repo2.Workflow(f.stream2, intake.DraftSubject)
		must(t, err)
		feature, err := repo1.Workflow(f.stream1, trace.FeatureSubject)
		must(t, err)
		return state.Value == intake.DraftExhausted && feature.Value == SketchedState
	})

	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	want := "member " + string(f.stream2) + " is handed: its drafting stopped after 3 refused drafts"
	for i, to := range []string{InShedState, SketchedState} {
		_, api := f.s.lifecycle().MoveWorkstream(ctx, repo1, f.stream1, WorkstreamMoveRequest{To: to, Note: "Start the review."}, chief, "chief-turn-"+string(rune('1'+i)))
		if err := asError(api); !failed(err, Conflict) || !containsMessage(err, want) || !containsMessage(err, "move it to sketched with a note") {
			t.Fatalf("move of the waiting member to %s: %v", to, api)
		}
	}
	_, api := f.s.lifecycle().MoveWorkstream(ctx, repo2, f.stream2, WorkstreamMoveRequest{To: InShedState, Note: "Start the review."}, chief, "chief-turn-3")
	if err := asError(api); !failed(err, Conflict) || !containsMessage(err, "has no accepted spec and plan to debate") {
		t.Fatalf("in-shed move of the stopped member: %v", api)
	}
	out, api := f.s.lifecycle().MoveWorkstream(ctx, repo2, f.stream2, WorkstreamMoveRequest{To: SketchedState, Note: "Draft the client's resume path."}, chief, "chief-turn-4")
	if api != nil {
		t.Fatal(api)
	}
	if out.State != HandedState || !strings.Contains(out.Detail, "writes draft 4") {
		t.Fatalf("stopped member's move: %+v", out)
	}
}
