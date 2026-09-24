package service

import (
	"context"
	"fmt"
	"slices"
	"testing"

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
	f, fake := newCappedMasonFixture(t, fmt.Sprintf("masons = %d\nper_workstream = %d\n", masons, perWorkstream), drafted)
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

// Two ready units of one workstream whose footprints are disjoint implement
// at once, each on its own workspace. A ready unit whose footprint intersects
// an implementing unit's waits, and starts once that unit's mason reported
// done.
func TestDisjointUnitsImplementConcurrently(t *testing.T) {
	t.Parallel()
	f, masons := newParallelMasonFixture(t, 4, 3, parallelPlan)
	defer f.stop(t)
	masons.play[masonTurnID("resume")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		args := map[string]any{"outcome": "Built resume", "criteria": []any{criterionArgs(CriterionReport{Criterion: "spec#1", Done: "built resume", Evidence: "the planned proof holds", Proof: "TestResume"})}}
		if recorded, reason, err := done(ctx, tools, args); err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	}
	stream, _ := f.builtAs(t, "parallel")
	f.awaitMasonRan(t, stream, "upload")
	f.awaitMasonRan(t, stream, "audit")
	f.awaitMasonRan(t, stream, "dedupe")
	settle()
	masons.check(t)

	// resume, upload and audit start in one pass; dedupe shares resume's
	// footprint and starts after resume left implementing.
	got := starts(t, f, stream)
	want := []string{trace.UnitSubject("resume"), trace.UnitSubject("upload"), trace.UnitSubject("audit"), trace.UnitSubject("dedupe")}
	if !slices.Equal(got, want) {
		t.Fatalf("started %v, want %v", got, want)
	}
	transitions := masonTransitions(t, f, stream)
	finished := slices.IndexFunc(transitions, func(tr transitionMove) bool {
		return tr.Subject == trace.UnitSubject("resume") && tr.To == UnitReviewing
	})
	dedupe := slices.IndexFunc(transitions, func(tr transitionMove) bool { return tr.ID == masonTransitionID("dedupe") })
	audit := slices.IndexFunc(transitions, func(tr transitionMove) bool { return tr.ID == masonTransitionID("audit") })
	if finished < 0 || audit > finished || dedupe < finished {
		t.Fatalf("resume left implementing at %d, audit started at %d and dedupe at %d: %+v", finished, audit, dedupe, transitions)
	}
	for _, unit := range []string{"upload", "audit", "dedupe"} {
		if state, err := f.unitState(stream, unit); err != nil || state != UnitImplementing {
			t.Fatalf("unit %s is %q: %v", unit, state, err)
		}
	}
}

// Units stop starting at the lower of the two caps: capacity.masons across
// workstreams, and capacity.per_workstream, which counts the workstream's
// implementing units whether or not their mason's turn is running.
func TestMasonStartsStayWithinBothCaps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                  string
		masons, perWorkstream int
		audit                 UnitDispatch
	}{
		{"capacity.masons", 2, 3, slotless(2)},
		{"capacity.per_workstream", 4, 2, UnitDispatch{Reason: DeferWorkstreamCap, Limit: 2, Message: "Waits for a slot in its workstream: 2 of its units are implementing, the per-workstream cap."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, masons := newParallelMasonFixture(t, tc.masons, tc.perWorkstream, disjointPlan)
			defer f.stop(t)
			stream, _ := f.builtAs(t, "capped")
			f.awaitMasonRan(t, stream, "resume")
			f.awaitMasonRan(t, stream, "upload")
			settle()
			masons.check(t)
			if got, want := starts(t, f, stream), []string{trace.UnitSubject("resume"), trace.UnitSubject("upload")}; !slices.Equal(got, want) {
				t.Fatalf("started %v, want %v", got, want)
			}
			f.checkUnits(t, stream, []UnitStatus{{Unit: "resume", State: UnitImplementing}, {Unit: "upload", State: UnitImplementing}, f.deferred(t, stream, "audit", tc.audit)})
		})
	}
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
	stream, _ := f.builtAs(t, "waiting")
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
