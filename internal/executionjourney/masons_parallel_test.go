package executionjourney

import (
	context "context"
	fmt "fmt"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMasonStartsStayWithinBothCaps(t *testing.T) {
	t.Parallel()
	f, masons := journey.NewParallelMasonFixture(t, 4, 2, intakefixture.DisjointPlan)
	defer f.Stop(t)
	stream := seedBuilding(f, t, "capped", intakefixture.DisjointPlan)
	f.AwaitMasonRan(t, stream, "resume")
	f.AwaitMasonRan(t, stream, "upload")
	awaitDeferral(f, t, stream, "audit", execution.DeferWorkstreamCap)
	masons.Check(t)
	if got, want := journey.Starts(t, f, stream), []string{trace.UnitSubject("resume"), trace.UnitSubject("upload")}; !slices.Equal(got, want) {
		t.Fatalf("started %v, want %v", got, want)
	}
	capped := api.UnitDispatch{Reason: execution.DeferWorkstreamCap, Limit: 2, Message: "Waits for a slot in its workstream: 2 of its units are implementing, the per-workstream cap."}
	f.CheckUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, {Unit: "upload", State: intake.UnitImplementing}, deferred(f, t, stream, "audit", capped)})
}

func TestWaitingUnitLeavesItsSlotToADisjointUnit(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	f, masons := journey.NewParallelMasonFixture(t, 2, 2, journey.ParallelPlan)
	defer f.Stop(t)
	chief := &intakefixture.Chief{P: p, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = chief.Turn
	f.Engine.Turns[execution.MasonTurnID("resume")] = masons.Asking(p, "", "1")
	f.Engine.Mu.Unlock()
	stream := seedBuilding(f, t, "waiting", journey.ParallelPlan)
	f.AwaitMasonRan(t, stream, "audit")
	awaitDeferral(f, t, stream, "dedupe", execution.DeferEntangled)
	p.Check(t)
	masons.Check(t)

	transitions := masonTransitions(t, f, stream)
	parkedAt := slices.Index(transitions, intakefixture.Parked("resume", "1"))
	audit := slices.IndexFunc(transitions, func(tr intakefixture.TransitionMove) bool { return tr.ID == execution.MasonTransitionID("audit") })
	if got, want := journey.Starts(t, f, stream), []string{trace.UnitSubject("resume"), trace.UnitSubject("upload"), trace.UnitSubject("audit")}; !slices.Equal(got, want) || parkedAt < 0 || audit < parkedAt {
		t.Fatalf("mason transitions %+v", transitions)
	}
	f.CheckUnits(t, stream, []api.UnitStatus{{Unit: "resume", State: intake.UnitWaiting}, {Unit: "upload", State: intake.UnitImplementing}, deferred(f, t, stream, "dedupe", overlapping("resume")), {Unit: "audit", State: intake.UnitImplementing}})
}

func TestMasonSessionsWithinCapacityRunAtOnce(t *testing.T) {
	t.Parallel()
	f, masons := journey.NewParallelMasonFixture(t, 2, 3, intakefixture.DisjointPlan)
	defer f.Stop(t)
	masons.Play[execution.MasonTurnID("resume")] = intakefixture.ReportDone("Built")
	var mu sync.Mutex
	started := map[string]bool{}
	both, release := make(chan struct{}), make(chan struct{})
	overlap := func(ctx context.Context, req agent.Request) error {
		mu.Lock()
		started[req.Name] = true
		if len(started) == 2 {
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Minute):
			t.Errorf("the session of %s never overlapped the other mason's", req.Name)
			return fmt.Errorf("the session of %s never overlapped the other mason's", req.Name)
		}
	}
	chief := &intakefixture.Chief{P: &intakefixture.Faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.Engine.Mu.Lock()
	f.Engine.Turns[execution.MasonTurnID("resume")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := overlap(ctx, req); err != nil {
			return nil, err
		}
		return masons.Turn(ctx, req, verified, tools)
	}
	f.Engine.Turns[execution.MasonTurnID("upload")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := overlap(ctx, req); err != nil {
			return nil, err
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return masons.Turn(ctx, req, verified, tools)
	}
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.Turn(ctx, req, verified, tools)
	}
	f.Engine.Mu.Unlock()
	stream := seedBuilding(f, t, "overlap", intakefixture.DisjointPlan)
	f.AwaitMerged(t, stream, "resume")
	th, err := f.Repository().Thread(stream, intake.MasonAgent("upload"))
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Claim == nil || !th.Turns[0].CompletedAt.IsZero() {
		t.Fatalf("the upload mason's turn is not in flight while resume lands: %+v", th.Turns)
	}
	close(release)
	f.AwaitMasonRan(t, stream, "upload")
	masons.Check(t)
}
