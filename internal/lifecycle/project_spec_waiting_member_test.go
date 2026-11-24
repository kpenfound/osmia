package lifecycle_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAWaitingMemberNamesTheMemberWhoseDraftingStopped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var allowed atomic.Value
	var released atomic.Bool
	f := newCrossRepoRatifyFixture(t, func(opts *journey.Options) {
		hold := opts.Reconciliation.Hold
		opts.Reconciliation.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
			if op.Action == intake.DraftAction && !released.Load() {
				if s, _ := allowed.Load().(config.WorkstreamID); s != stream {
					return true
				}
			}
			return hold != nil && hold(stream, op)
		}
	})
	defer f.Stop(t)
	f.Engine.Mu.Lock()
	scripted := f.Engine.Turns["draft-1-1"]
	f.Engine.Turns["draft-1-1"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Workspace.Directory(), string(f.Stream2)) {
			return nil, errors.New("the architect is unavailable")
		}
		return scripted(ctx, req, turn, tools)
	}
	f.Engine.Mu.Unlock()
	if _, apiFault := f.S.Lifecycle().DraftWorkgroup(ctx, []config.WorkstreamID{f.Stream1, f.Stream2}); apiFault != nil {
		t.Fatal(apiFault)
	}
	allowed.Store(f.Stream1)
	released.Store(true)
	repo1, repo2 := mustRepo(t, f.S, f.Project), mustRepo(t, f.S, f.Project2)
	repo1.WakeWorkflow()
	repo2.WakeWorkflow()
	intakefixture.Eventually(t, "the second member's drafting stops", func() bool {
		state, err := repo2.Workflow(f.Stream2, intake.DraftSubject)
		fixturekit.Must(t, err)
		feature, err := repo1.Workflow(f.Stream1, trace.FeatureSubject)
		fixturekit.Must(t, err)
		return state.Value == intake.DraftExhausted && feature.Value == intake.SketchedState
	})

	chief := trace.Actor{Kind: "agent", ID: trace.ChiefOfStaff}
	want := "member " + string(f.Stream2) + " is handed: its drafting stopped after 3 refused drafts"
	for i, to := range []string{intake.InShedState, intake.SketchedState} {
		_, apiFault := f.S.Lifecycle().MoveWorkstream(ctx, repo1, f.Stream1, api.WorkstreamMoveRequest{To: to, Note: "Start the review."}, chief, "chief-turn-"+string(rune('1'+i)))
		if err := asError(apiFault); !intakefixture.Failed(err, api.Conflict) || !containsMessage(err, want) || !containsMessage(err, "move it to sketched with a note") {
			t.Fatalf("move of the waiting member to %s: %v", to, apiFault)
		}
	}
	_, apiFault := f.S.Lifecycle().MoveWorkstream(ctx, repo2, f.Stream2, api.WorkstreamMoveRequest{To: intake.InShedState, Note: "Start the review."}, chief, "chief-turn-3")
	if err := asError(apiFault); !intakefixture.Failed(err, api.Conflict) || !containsMessage(err, "has no accepted spec and plan to debate") {
		t.Fatalf("in-shed move of the stopped member: %v", apiFault)
	}
	out, apiFault := f.S.Lifecycle().MoveWorkstream(ctx, repo2, f.Stream2, api.WorkstreamMoveRequest{To: intake.SketchedState, Note: "Draft the client's resume path."}, chief, "chief-turn-4")
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	if out.State != intake.HandedState || !strings.Contains(out.Detail, "writes draft 4") {
		t.Fatalf("stopped member's move: %+v", out)
	}
}
