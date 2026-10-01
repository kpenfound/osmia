package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

// parallelPlan has resume and dedupe in internal.trace, and upload and audit
// in internal.upload and internal.audit, disjoint from it and from each other.
// No unit depends on another.
const parallelPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "upload", "title": "Send chunks", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestUpload"}}], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": [], "footprint": ["internal.audit"]}
]}
`

// disjointPlan has three units whose footprints are pairwise disjoint.
const disjointPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "upload", "title": "Send chunks", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestUpload"}}], "depends_on": [], "footprint": ["internal.upload"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": [], "footprint": ["internal.audit"]}
]}
`

// newParallelMasonFixture is a mason fixture with capacity.masons set to
// masons and capacity.per_workstream to perWorkstream, whose entity map adds
// internal.upload and internal.audit to the seeded one before any plan is
// drafted. Every unit's first mason turn is played by the returned fake
// masons.
func newParallelMasonFixture(t *testing.T, masons, perWorkstream int, drafted string) (*shedFixture, *fakeMasons) {
	t.Helper()
	return newParallelMasonFixtureOn(t, config.WorkspacesGit, masons, perWorkstream, drafted)
}

// newParallelMasonFixtureOn is newParallelMasonFixture whose workstreams are
// handed in on the workspace backend given.
func newParallelMasonFixtureOn(t *testing.T, backend string, masons, perWorkstream int, drafted string) (*shedFixture, *fakeMasons) {
	t.Helper()
	f, fake := newMasonFixtureOn(t, backend, fmt.Sprintf("masons = %d\nper_workstream = %d\n", masons, perWorkstream), drafted, "")
	mapping, err := kb.Load(f.repository())
	must(t, err)
	for _, name := range []string{"upload", "audit"} {
		mapping.Entities = append(mapping.Entities, kb.Entity{ID: "internal." + name, Name: "internal/" + name, Aliases: []string{}, Paths: []string{"internal/" + name}, Owners: []string{}, PartOf: []string{}})
	}
	must(t, kb.Store(context.Background(), f.repository(), mapping, f.clock.Now(), librarianActor, "fixture"))
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	for _, unit := range []string{"upload", "audit"} {
		f.engine.turns[masonTurnID(unit)] = fake.turn
	}
	return f, fake
}

// starts returns the units the mason controller started in the workstream,
// in the order it started them.
func starts(t *testing.T, f *shedFixture, stream config.WorkstreamID) []string {
	t.Helper()
	var out []string
	for _, tr := range masonTransitions(t, f, stream) {
		if tr.From == UnitReady && tr.To == UnitImplementing {
			out = append(out, tr.Subject)
		}
	}
	return out
}

// Units stop starting at capacity.per_workstream, which counts the
// workstream's implementing units whether or not their mason's turn is
// running, even with mason slots free.
func TestMasonStartsStayWithinBothCaps(t *testing.T) {
	t.Parallel()
	f, masons := newParallelMasonFixture(t, 4, 2, disjointPlan)
	defer f.stop(t)
	stream := f.seedBuilding(t, "capped", disjointPlan)
	f.awaitMasonRan(t, stream, "resume")
	f.awaitMasonRan(t, stream, "upload")
	settle()
	masons.check(t)
	if got, want := starts(t, f, stream), []string{trace.UnitSubject("resume"), trace.UnitSubject("upload")}; !slices.Equal(got, want) {
		t.Fatalf("started %v, want %v", got, want)
	}
	capped := UnitDispatch{Reason: DeferWorkstreamCap, Limit: 2, Message: "Waits for a slot in its workstream: 2 of its units are implementing, the per-workstream cap."}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "upload", State: UnitImplementing}, f.deferred(t, stream, "audit", capped)})
}

// A unit whose mason asked a question takes neither a mason slot nor a place
// under capacity.per_workstream, so a disjoint ready unit starts in its
// place; a ready unit sharing its footprint does not.
func TestWaitingUnitLeavesItsSlotToADisjointUnit(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, masons := newParallelMasonFixture(t, 2, 2, parallelPlan)
	defer f.stop(t)
	chief := &chief{p: p, released: map[string]bool{}, held: map[string]chan struct{}{}}
	f.engine.mu.Lock()
	f.engine.turns["*"] = chief.turn
	f.engine.turns[masonTurnID("resume")] = masons.asking(p, "", "1")
	f.engine.mu.Unlock()
	stream := f.seedBuilding(t, "waiting", parallelPlan)
	f.awaitMasonRan(t, stream, "audit")
	settle()
	p.check(t)
	masons.check(t)

	transitions := masonTransitions(t, f, stream)
	parkedAt := slices.Index(transitions, parked("resume", "1"))
	audit := slices.IndexFunc(transitions, func(tr transitionMove) bool { return tr.ID == masonTransitionID("audit") })
	if got, want := starts(t, f, stream), []string{trace.UnitSubject("resume"), trace.UnitSubject("upload"), trace.UnitSubject("audit")}; !slices.Equal(got, want) || parkedAt < 0 || audit < parkedAt {
		t.Fatalf("mason transitions %+v", transitions)
	}
	f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitWaiting}, {Unit: "upload", State: UnitImplementing}, f.deferred(t, stream, "dedupe", overlapping("resume")), {Unit: "audit", State: UnitImplementing}})
}

// Mason turns dispatched within capacity.masons run at the same time: each
// unit's session waits until the other's has started. While the upload
// mason's session is still running, resume's candidate is reviewed and
// landed.
func TestMasonSessionsWithinCapacityRunAtOnce(t *testing.T) {
	t.Parallel()
	f, masons := newParallelMasonFixture(t, 2, 3, disjointPlan)
	defer f.stop(t)
	masons.play[masonTurnID("resume")] = reportDone("Built")
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
	chief := &chief{p: &faults{}, released: map[string]bool{}, held: map[string]chan struct{}{}}
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := overlap(ctx, req); err != nil {
			return nil, err
		}
		return masons.turn(ctx, req, verified, tools)
	}
	f.engine.turns[masonTurnID("upload")] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := overlap(ctx, req); err != nil {
			return nil, err
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return masons.turn(ctx, req, verified, tools)
	}
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.turn(ctx, req, verified, tools)
	}
	f.engine.mu.Unlock()
	stream := f.seedBuilding(t, "overlap", disjointPlan)
	f.awaitMerged(t, stream, "resume")
	th, err := f.repository().Thread(stream, masonAgent("upload"))
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Claim == nil || !th.Turns[0].CompletedAt.IsZero() {
		t.Fatalf("the upload mason's turn is not in flight while resume lands: %+v", th.Turns)
	}
	close(release)
	f.awaitMasonRan(t, stream, "upload")
	masons.check(t)
}
