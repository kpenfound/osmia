package executionjourney

import (
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	slices "slices"
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"

	plan "github.com/kpenfound/osmia/internal/plan"
	runtime "github.com/kpenfound/osmia/internal/runtime"
)

func (f *shedFixture) deferred(t *testing.T, stream config.WorkstreamID, unit string, d UnitDispatch) UnitStatus {
	t.Helper()
	state, _ := f.s.store.Effective()
	return intakefixture.DeferredUnit(t, f.repository(), state.Pauses, stream, unit, d)
}

func overlapping(unit string) UnitDispatch {
	return UnitDispatch{Reason: DeferEntangled, Blockers: []plan.StartBlocker{{Unit: unit, Reason: plan.OverlapReason}}, Message: "Waits for units in flight it is entangled with: " + unit + " (their footprints overlap)."}
}

func slotless(masons int) UnitDispatch {
	return UnitDispatch{Reason: DeferCapacity, Limit: masons, Message: fmt.Sprintf("Waits for a mason slot: all %d are in use.", masons)}
}

var factoryPaused = UnitDispatch{Reason: DeferPaused, Pause: &runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: "owner"}, Message: "Waits while a factory pause is in force, set by owner."}

func blockedOnItself(unit string) UnitDispatch {
	return UnitDispatch{Reason: DeferBlocked, Blocked: unit, Message: "Cannot start: the mason controller is blocked on this unit and tries again on every pass."}
}

func (f *shedFixture) dispatches(t *testing.T, stream config.WorkstreamID, unit string) []string {
	t.Helper()
	return intakefixture.DispatchDecisions(t, f.repository(), stream, unit)
}

func (f *shedFixture) awaitDispatches(t *testing.T, stream config.WorkstreamID, unit string, n int) {
	t.Helper()
	intakefixture.AwaitDispatchDecisions(t, f.repository(), stream, unit, n)
}

func TestReadyUnitDecisionsFollowPauseAndPriority(t *testing.T) {
	t.Parallel()
	f, masons := newParallelMasonFixture(t, 1, 3, disjointPlan)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	built := f.builtPaused(t, factory, "first", "second")
	a, b := built[0], built[1]

	lo, hi := lowHigh(a, b)
	units := []string{"resume", "upload", "audit"}
	for _, stream := range []config.WorkstreamID{lo, hi} {
		f.awaitDispatches(t, stream, "audit", 1)
	}
	for _, stream := range []config.WorkstreamID{lo, hi} {
		var want []UnitStatus
		for _, unit := range units {
			want = append(want, f.deferred(t, stream, unit, factoryPaused))
		}
		f.checkUnits(t, stream, want)
	}

	mutation(t, f.c, "PUT", "priority", PriorityRequest{Project: f.project, Workstreams: []config.WorkstreamID{hi, lo}})
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMasonRan(t, hi, "resume")
	for _, unit := range units {
		f.awaitDispatches(t, lo, unit, 2)
	}
	behind := UnitDispatch{Reason: DeferPriority, Limit: 1, Workstreams: []config.WorkstreamID{hi}, Message: "Waits for a mason slot: all 1 are in use, and higher-priority workstreams start first: " + string(hi) + "."}
	f.checkUnits(t, hi, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, hi, "upload", slotless(1)), f.deferred(t, hi, "audit", slotless(1))})
	f.checkUnits(t, lo, []UnitStatus{f.deferred(t, lo, "resume", behind), f.deferred(t, lo, "upload", behind), f.deferred(t, lo, "audit", behind)})
	for _, unit := range units {
		if got, want := f.dispatches(t, lo, unit), []string{"deferred paused", "deferred priority"}; !slices.Equal(got, want) {
			t.Fatalf("decisions on %s of %s: %q, want %q", unit, lo, got, want)
		}
	}
	if got, want := f.dispatches(t, hi, "resume"), []string{"deferred paused", "started"}; !slices.Equal(got, want) {
		t.Fatalf("decisions on resume of %s: %q, want %q", hi, got, want)
	}

	pause := runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: hi}, Mode: "soft", Source: "owner"}
	mutation(t, f.c, "PUT", "pause", PauseRequest(pause))
	f.awaitMasonRan(t, lo, "resume")
	decisions := map[config.WorkstreamID]map[string][]string{
		lo: {"resume": {"deferred paused", "deferred priority", "started"}, "upload": {"deferred paused", "deferred priority", "deferred capacity"}},
		hi: {"resume": {"deferred paused", "started"}, "upload": {"deferred paused", "deferred capacity", "deferred paused"}},
	}
	for stream, want := range decisions {
		for unit, want := range want {
			f.awaitDispatches(t, stream, unit, len(want))
		}
	}
	masons.Check(t)
	paused := UnitDispatch{Reason: DeferPaused, Pause: &pause, Message: "Waits while a workstream pause is in force, set by owner."}
	f.checkUnits(t, hi, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, hi, "upload", paused), f.deferred(t, hi, "audit", paused)})
	f.checkUnits(t, lo, []UnitStatus{{Unit: "resume", State: UnitImplementing}, f.deferred(t, lo, "upload", slotless(1)), f.deferred(t, lo, "audit", slotless(1))})
	for stream, want := range decisions {
		for unit, want := range want {
			if got := f.dispatches(t, stream, unit); !slices.Equal(got, want) {
				t.Fatalf("decisions on %s of %s: %q, want %q", unit, stream, got, want)
			}
		}
	}
}
