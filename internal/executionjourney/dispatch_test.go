package executionjourney

import (
	context "context"
	fmt "fmt"
	slices "slices"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	plan "github.com/kpenfound/osmia/internal/plan"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func deferred(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, unit string, d api.UnitDispatch) api.UnitStatus {
	t.Helper()
	state, _ := f.S.Store.Effective()
	return intakefixture.DeferredUnit(t, f.Repository(), state.Pauses, stream, unit, d)
}

func overlapping(unit string) api.UnitDispatch {
	return api.UnitDispatch{Reason: execution.DeferEntangled, Blockers: []plan.StartBlocker{{Unit: unit, Reason: plan.OverlapReason}}, Message: "Waits for units in flight it is entangled with: " + unit + " (their footprints overlap)."}
}

func slotless(masons int) api.UnitDispatch {
	return api.UnitDispatch{Reason: execution.DeferCapacity, Limit: masons, Message: fmt.Sprintf("Waits for a mason slot: all %d are in use.", masons)}
}

var factoryPaused = api.UnitDispatch{Reason: execution.DeferPaused, Pause: &runtime.Pause{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: "owner"}, Message: "Waits while a factory pause is in force, set by owner."}

func blockedOnItself(unit string) api.UnitDispatch {
	return api.UnitDispatch{Reason: execution.DeferBlocked, Blocked: unit, Message: "Cannot start: the mason controller is blocked on this unit and tries again on every pass."}
}

func dispatches(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, unit string) []string {
	t.Helper()
	return intakefixture.DispatchDecisions(t, f.Repository(), stream, unit)
}

func awaitDispatches(f *journey.ShedFixture, t *testing.T, stream config.WorkstreamID, unit string, n int) {
	t.Helper()
	intakefixture.AwaitDispatchDecisions(t, f.Repository(), stream, unit, n)
}

func TestReadyUnitDecisionsFollowPauseAndPriority(t *testing.T) {
	t.Parallel()

	f, masons := journey.NewParallelMasonFixturePrepared(t, config.WorkspacesGit, 1, 3, intakefixture.DisjointPlan, func(opts *journey.Options) {
		journey.AppendConfig(t, opts, "\n[mason]\nmax_clean_turns = 20\n")
	})
	defer f.Stop(t)

	hiPaused := make(chan struct{})
	running := make(chan struct{}, 16)
	continued := func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		select {
		case <-hiPaused:
			<-ctx.Done()
			return nil, ctx.Err()
		default:
		}
		select {
		case running <- struct{}{}:
		default:
		}
		select {
		case <-hiPaused:
			return masons.Turn(ctx, req, turn, tools)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.Engine.Mu.Lock()
	for _, unit := range []string{"resume", "upload", "audit"} {
		for i := 1; i <= 6; i++ {
			f.Engine.Turns[fmt.Sprintf("%s-continue-%d", intake.MasonAgent(unit), i)] = continued
			f.Engine.Turns[fmt.Sprintf("%s-recover-%d", intake.MasonAgent(unit), i)] = continued
		}
	}
	f.Engine.Mu.Unlock()
	factory := runtime.Target{Scope: "factory"}
	built := builtPaused(f, t, factory, "first", "second")
	a, b := built[0], built[1]

	lo, hi := intakefixture.LowHigh(a, b)
	units := []string{"resume", "upload", "audit"}
	for _, stream := range []config.WorkstreamID{lo, hi} {
		awaitDispatches(f, t, stream, "audit", 1)
	}
	for _, stream := range []config.WorkstreamID{lo, hi} {
		var want []api.UnitStatus
		for _, unit := range units {
			want = append(want, deferred(f, t, stream, unit, factoryPaused))
		}
		f.CheckUnits(t, stream, want)
	}

	intakefixture.Mutation(t, f.C, "PUT", "priority", api.PriorityRequest{Project: f.Project, Workstreams: []config.WorkstreamID{hi, lo}})
	intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)
	f.AwaitMasonRan(t, hi, "resume")
	for _, unit := range units {
		awaitDispatches(f, t, lo, unit, 2)
	}
	behind := api.UnitDispatch{Reason: execution.DeferPriority, Limit: 1, Workstreams: []config.WorkstreamID{hi}, Message: "Waits for a mason slot: all 1 are in use, and higher-priority workstreams start first: " + string(hi) + "."}
	f.CheckUnits(t, hi, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, deferred(f, t, hi, "upload", slotless(1)), deferred(f, t, hi, "audit", slotless(1))})
	f.CheckUnits(t, lo, []api.UnitStatus{deferred(f, t, lo, "resume", behind), deferred(f, t, lo, "upload", behind), deferred(f, t, lo, "audit", behind)})
	for _, unit := range units {
		if got, want := dispatches(f, t, lo, unit), []string{"deferred paused", "deferred priority"}; !slices.Equal(got, want) {
			t.Fatalf("decisions on %s of %s: %q, want %q", unit, lo, got, want)
		}
	}
	if got, want := dispatches(f, t, hi, "resume"), []string{"deferred paused", "started"}; !slices.Equal(got, want) {
		t.Fatalf("decisions on resume of %s: %q, want %q", hi, got, want)
	}

	select {
	case <-running:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatalf("no continuation of a mason of %s started before the pause", hi)
	}
	pause := runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: f.Project, Workstream: hi}, Mode: "soft", Source: "owner"}
	intakefixture.Mutation(t, f.C, "PUT", "pause", api.PauseRequest(pause))
	close(hiPaused)
	f.AwaitMasonRan(t, lo, "resume")
	decisions := map[config.WorkstreamID]map[string][]string{
		lo: {"resume": {"deferred paused", "deferred priority", "started"}, "upload": {"deferred paused", "deferred priority", "deferred capacity"}},
		hi: {"resume": {"deferred paused", "started"}, "upload": {"deferred paused", "deferred capacity", "deferred paused"}},
	}
	for stream, want := range decisions {
		for unit, want := range want {
			awaitDispatches(f, t, stream, unit, len(want))
		}
	}
	masons.Check(t)
	paused := api.UnitDispatch{Reason: execution.DeferPaused, Pause: &pause, Message: "Waits while a workstream pause is in force, set by owner."}
	f.CheckUnits(t, hi, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, deferred(f, t, hi, "upload", paused), deferred(f, t, hi, "audit", paused)})
	f.CheckUnits(t, lo, []api.UnitStatus{{Unit: "resume", State: intake.UnitImplementing}, deferred(f, t, lo, "upload", slotless(1)), deferred(f, t, lo, "audit", slotless(1))})
	for stream, want := range decisions {
		for unit, want := range want {
			if got := dispatches(f, t, stream, unit); !slices.Equal(got, want) {
				t.Fatalf("decisions on %s of %s: %q, want %q", unit, stream, got, want)
			}
		}
	}
}
