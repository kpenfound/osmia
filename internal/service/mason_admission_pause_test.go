package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestMasonAdmissionUsesOnePauseSnapshotForSlotsAndDecisions(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"pause", "unpause"} {
		t.Run(change, func(t *testing.T) {
			f, _ := newParallelMasonFixture(t, 1, 3, disjointPlan)
			defer f.stop(t)
			built := f.builtPaused(t, factoryTarget, "first", "second")
			lo, hi := lowHigh(built[0], built[1])
			mutation(t, f.c, "PUT", "priority", PriorityRequest{Project: f.project, Workstreams: []config.WorkstreamID{hi, lo}})
			f.awaitDispatches(t, lo, "resume", 1)

			// Run the admission pass directly so the next pass cannot hide
			// a transient capacity decision or admit a second worker for it.
			_, projects := f.s.runtimes()
			active := projects[0]
			active.controller.Drain()
			select {
			case err := <-active.done:
				must(t, err)
			case <-time.After(demoTimeout):
				t.Fatal("controller did not drain")
			}
			must(t, f.s.store.ClearPause(factoryTarget, "owner"))
			pause := runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: hi}, Mode: "soft", Source: "owner", Reason: "Hold the higher-priority workstream"}
			if change == "unpause" {
				must(t, f.s.store.SetPause(pause))
			}
			m := &masons{s: f.s, cfg: active.pipeline.current.Load().cfg, repository: f.repository()}
			b, found, err := m.read(hi)
			must(t, err)
			if !found {
				t.Fatal("building workstream missing")
			}
			interrupted := errors.New("stop before the first turn is queued")
			f.s.setBoundary(func(name string) error {
				if name == "mason-implementing:"+string(hi)+":resume" {
					return interrupted
				}
				return nil
			})
			_, _, err = m.start(context.Background(), b, "resume")
			if !errors.Is(err, interrupted) {
				t.Fatalf("start: %v", err)
			}
			changed := false
			f.s.setBoundary(func(name string) error {
				if name != "mason-turn-queued:"+string(hi)+":resume" {
					return nil
				}
				changed = true
				if change == "pause" {
					return f.s.store.SetPause(pause)
				}
				return f.s.store.ClearPause(pause.Target, "owner")
			})
			defer func() { f.s.setBoundary(nil) }()
			must(t, m.Pass(context.Background()))
			if !changed {
				t.Fatal("pause did not change during the mason scan")
			}
			wantState, wantDecision := UnitImplementing, "started"
			if change == "unpause" {
				wantState, wantDecision = UnitReady, "deferred priority"
			}
			state, err := f.repository().Workflow(lo, trace.UnitSubject("resume"))
			must(t, err)
			if state.Value != wantState {
				t.Fatalf("lower-priority unit is %s after %s, want %s", state.Value, change, wantState)
			}
			if got, want := f.dispatches(t, lo, "resume"), []string{"deferred paused", wantDecision}; !slices.Equal(got, want) {
				t.Fatalf("dispatches after %s: %v, want %v", change, got, want)
			}
		})
	}
}
