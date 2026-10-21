package executionjourney

import (
	context "context"
	fmt "fmt"
	slices "slices"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const parallelPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "upload", "title": "Send chunks", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestUpload"}}], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": [], "footprint": ["internal.audit"]}
]}
`

const disjointPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "upload", "title": "Send chunks", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestUpload"}}], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": [], "footprint": ["internal.audit"]}
]}
`

func newParallelMasonFixture(t *testing.T, masons, perWorkstream int, drafted string) (*shedFixture, *fakeMasons) {
	t.Helper()
	return newParallelMasonFixtureOn(t, config.WorkspacesGit, masons, perWorkstream, drafted)
}

func newParallelMasonFixtureOn(t *testing.T, backend string, masons, perWorkstream int, drafted string) (*shedFixture, *fakeMasons) {
	t.Helper()
	f, fake := newMasonFixtureOn(t, backend, fmt.Sprintf("masons = %d\nper_workstream = %d\n", masons, perWorkstream), drafted, "")
	intakefixture.ExtendParallelMasons(t, f.repository(), f.clock.Now(), f.engine, fake, masonTurnID, librarianActor)
	return f, fake
}

func starts(t *testing.T, f *shedFixture, stream config.WorkstreamID) []string {
	t.Helper()
	return intakefixture.MasonStarts(allTransitions(t, f.s.cfg.Root, f.project, stream))
}

func TestMasonStartsStayWithinBothCaps(t *testing.T) {
	t.Parallel()
	f, masons := newParallelMasonFixture(t, 4, 2, disjointPlan)
	defer f.stop(t)
	stream := f.seedBuilding(t, "capped", disjointPlan)
	f.awaitMasonRan(t, stream, "resume")
	f.awaitMasonRan(t, stream, "upload")
	f.awaitDeferral(t, stream, "audit", DeferWorkstreamCap)
	masons.Check(t)
	if got, want := starts(t, f, stream), []string{trace.UnitSubject("resume"), trace.UnitSubject("upload")}; !slices.Equal(got, want) {
		t.Fatalf("started %v, want %v", got, want)
	}
	capped := UnitDispatch{Reason: DeferWorkstreamCap, Limit: 2, Message: "Waits for a slot in its workstream: 2 of its units are implementing, the per-workstream cap."}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "upload", State: UnitImplementing}, f.deferred(t, stream, "audit", capped)})
}

func TestWaitingUnitLeavesItsSlotToADisjointUnit(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, masons := newParallelMasonFixture(t, 2, 2, parallelPlan)
	defer f.stop(t)
	chief := &chief{P: p, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = chief.Turn
	f.engine.Turns[masonTurnID("resume")] = masons.Asking(p, "", "1")
	f.engine.Mu.Unlock()
	stream := f.seedBuilding(t, "waiting", parallelPlan)
	f.awaitMasonRan(t, stream, "audit")
	f.awaitDeferral(t, stream, "dedupe", DeferEntangled)
	p.Check(t)
	masons.Check(t)

	transitions := masonTransitions(t, f, stream)
	parkedAt := slices.Index(transitions, parked("resume", "1"))
	audit := slices.IndexFunc(transitions, func(tr transitionMove) bool { return tr.ID == masonTransitionID("audit") })
	if got, want := starts(t, f, stream), []string{trace.UnitSubject("resume"), trace.UnitSubject("upload"), trace.UnitSubject("audit")}; !slices.Equal(got, want) || parkedAt < 0 || audit < parkedAt {
		t.Fatalf("mason transitions %+v", transitions)
	}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitWaiting}, {Unit: "upload", State: UnitImplementing}, f.deferred(t, stream, "dedupe", overlapping("resume")), {Unit: "audit", State: UnitImplementing}})
}

func TestMasonSessionsWithinCapacityRunAtOnce(t *testing.T) {
	t.Parallel()
	f, masons := newParallelMasonFixture(t, 2, 3, disjointPlan)
	defer f.stop(t)
	masons.Play[masonTurnID("resume")] = reportDone("Built")
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
	chief := &chief{P: &faults{}, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.engine.Mu.Lock()
	f.engine.Turns[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := overlap(ctx, req); err != nil {
			return nil, err
		}
		return masons.Turn(ctx, req, verified, tools)
	}
	f.engine.Turns[masonTurnID("upload")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
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
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.Turn(ctx, req, verified, tools)
	}
	f.engine.Mu.Unlock()
	stream := f.seedBuilding(t, "overlap", disjointPlan)
	f.awaitMerged(t, stream, "resume")
	th, err := f.repository().Thread(stream, masonAgent("upload"))
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Claim == nil || !th.Turns[0].CompletedAt.IsZero() {
		t.Fatalf("the upload mason's turn is not in flight while resume lands: %+v", th.Turns)
	}
	close(release)
	f.awaitMasonRan(t, stream, "upload")
	masons.Check(t)
}
