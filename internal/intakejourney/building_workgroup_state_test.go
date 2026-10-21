package intakejourney

import (
	context "context"
	fmt "fmt"
	reflect "reflect"
	strings "strings"
	testing "testing"

	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestCrossRepoBuildPreservesConcurrentFeatureTransition(t *testing.T) {
	t.Parallel()
	for _, injected := range []string{AbandonedState, DeliveredState, ""} {
		name := injected
		if name == "" {
			name = "unraced"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			gate := &changeRaceGate{Reached: make(chan struct{}), Released: make(chan struct{})}
			t.Cleanup(gate.Release)
			f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: gate.Checkpoint})
			defer f.stop(t)
			f.startOwner(t)
			packet := f.awaitPacket(t)
			gate.Step = "build-workflow-checked:" + string(f.stream1)
			gate.Armed.Store(true)
			_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			must(t, err)
			gate.Await(t)

			repo, err := f.s.repository(f.project)
			must(t, err)
			transitions, err := trace.Read[trace.Transition](repo, f.stream1)
			must(t, err)
			var raw string
			for _, tr := range transitions {
				if tr.Subject == trace.FeatureSubject {
					raw = tr.To
				}
			}
			if raw != trace.SealPreparedState {
				t.Fatalf("raw state at build checkpoint = %q, want seal-prepared", raw)
			}
			effective, err := repo.Workflow(f.stream1, trace.FeatureSubject)
			must(t, err)
			if effective.Value != RatifiedState {
				t.Fatalf("effective state at build checkpoint = %q, want ratified", effective.Value)
			}
			switch injected {
			case AbandonedState:
				_, err = f.c.Abandon(ctx, f.stream1, "Owner cancelled before the build recorded units.")
				must(t, err)
			case DeliveredState:
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "concurrent-delivery", Revision: 1, Project: f.project, Workstream: f.stream1, At: f.clock.Now(), Actor: ownerActor, Cause: "fixture"}
				_, err = repo.SetFeatureState(ctx, h, DeliveredState, "Owner-approved publication completed")
				must(t, err)
			}
			gate.Release()

			var completed trace.OperationRecord
			lifecycleAwait(t, f, "cross-repo build settled", func() bool {
				ops, err := repo.Operations(f.stream1)
				must(t, err)
				for _, op := range ops {
					if op.Operation.Action == BuildAction && op.Acknowledged {
						completed = op
						return true
					}
				}
				return false
			})
			wantState, wantOutcome := injected, "failed"
			if injected == "" {
				wantState, wantOutcome = BuildingState, "succeeded"
			}
			if completed.Result == nil || completed.Result.Outcome != wantOutcome {
				t.Fatalf("build result = %+v, want %s", completed.Result, wantOutcome)
			}
			if injected != "" && completed.Result.Evidence != fmt.Sprintf("building on seal 1 failed: the workflow changed while the build ran; the workstream is %s", featureState(injected)) {
				t.Fatalf("build did not reject the concurrent transition: %+v", completed.Result)
			}
			for _, a := range completed.History {
				if a.Kind == "retry" {
					t.Fatalf("build retried: %+v", a)
				}
			}
			check := func() {
				t.Helper()
				repo, err := f.s.repository(f.project)
				must(t, err)
				state, err := repo.Workflow(f.stream1, trace.FeatureSubject)
				must(t, err)
				if state.Value != wantState {
					t.Fatalf("feature state = %s, want %s", state.Value, wantState)
				}
				transitions, err := trace.Read[trace.Transition](repo, f.stream1)
				must(t, err)
				unitStates, builds := 0, 0
				for _, tr := range transitions {
					if strings.HasPrefix(tr.Subject, "unit-") {
						unitStates++
					}
					if tr.Subject == trace.FeatureSubject && tr.To == BuildingState {
						builds++
						if tr.From != trace.SealPreparedState || tr.Cause != completed.Operation.ID {
							t.Fatalf("build used the wrong preparation or operation: %+v", tr)
						}
					}
				}
				if injected != "" && (unitStates != 0 || builds != 0) {
					t.Fatalf("rejected build recorded %d unit states and %d building transitions", unitStates, builds)
				}
				if injected == "" && (unitStates == 0 || builds != 1) {
					t.Fatalf("unraced build recorded %d unit states and %d building transitions", unitStates, builds)
				}
				ops, err := repo.Operations(f.stream1)
				must(t, err)
				count := 0
				for _, op := range ops {
					if op.Operation.Action == BuildAction {
						count++
						if !reflect.DeepEqual(op, completed) {
							t.Fatalf("settled build changed or retried: %+v", op)
						}
					}
				}
				if count != 1 {
					t.Fatalf("seal has %d builds, want one", count)
				}
			}
			check()
			f.stop(t)
			f.start(t)
			f.awaitApproved(t)
			f.checkContinuation(t)
			check()
		})
	}
}
