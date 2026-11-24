package intakejourney

import (
	context "context"
	fmt "fmt"
	reflect "reflect"
	strings "strings"
	testing "testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestCrossRepoBuildPreservesConcurrentFeatureTransition(t *testing.T) {
	t.Parallel()
	for _, injected := range []string{intake.AbandonedState, intake.DeliveredState, ""} {
		name := injected
		if name == "" {
			name = "unraced"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			gate := &intakefixture.ChangeRaceGate{Reached: make(chan struct{})}
			t.Cleanup(gate.Release)
			f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait, Checkpoint: gate.Checkpoint})
			defer f.Stop(t)
			f.StartOwner(t)
			packet := f.AwaitPacket(t)
			gate.Step = "build-workflow-checked:" + string(f.Stream1)
			gate.Armed.Store(true)
			_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			fixturekit.Must(t, err)
			gate.Await(t)

			repo, err := f.S.Repository(f.Project)
			fixturekit.Must(t, err)
			transitions, err := trace.Read[trace.Transition](repo, f.Stream1)
			fixturekit.Must(t, err)
			var raw string
			for _, tr := range transitions {
				if tr.Subject == trace.FeatureSubject {
					raw = tr.To
				}
			}
			if raw != trace.SealPreparedState {
				t.Fatalf("raw state at build checkpoint = %q, want seal-prepared", raw)
			}
			effective, err := repo.Workflow(f.Stream1, trace.FeatureSubject)
			fixturekit.Must(t, err)
			if effective.Value != intake.RatifiedState {
				t.Fatalf("effective state at build checkpoint = %q, want ratified", effective.Value)
			}
			switch injected {
			case intake.AbandonedState:
				_, err = f.C.Abandon(ctx, f.Stream1, "Owner cancelled before the build recorded units.")
				fixturekit.Must(t, err)
			case intake.DeliveredState:
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "concurrent-delivery", Revision: 1, Project: f.Project, Workstream: f.Stream1, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "fixture"}
				_, err = repo.SetFeatureState(ctx, h, intake.DeliveredState, "Owner-approved publication completed")
				fixturekit.Must(t, err)
			}
			gate.Release()

			var completed trace.OperationRecord
			journey.LifecycleAwait(t, f, "cross-repo build settled", func() bool {
				ops, err := repo.Operations(f.Stream1)
				fixturekit.Must(t, err)
				for _, op := range ops {
					if op.Operation.Action == intake.BuildAction && op.Acknowledged {
						completed = op
						return true
					}
				}
				return false
			})
			wantState, wantOutcome := injected, "failed"
			if injected == "" {
				wantState, wantOutcome = intake.BuildingState, "succeeded"
			}
			if completed.Result == nil || completed.Result.Outcome != wantOutcome {
				t.Fatalf("build result = %+v, want %s", completed.Result, wantOutcome)
			}
			if injected != "" && completed.Result.Evidence != fmt.Sprintf("building on seal 1 failed: the workflow changed while the build ran; the workstream is %s", intake.FeatureState(injected)) {
				t.Fatalf("build did not reject the concurrent transition: %+v", completed.Result)
			}
			for _, a := range completed.History {
				if a.Kind == "retry" {
					t.Fatalf("build retried: %+v", a)
				}
			}
			check := func() {
				t.Helper()
				repo, err := f.S.Repository(f.Project)
				fixturekit.Must(t, err)
				state, err := repo.Workflow(f.Stream1, trace.FeatureSubject)
				fixturekit.Must(t, err)
				if state.Value != wantState {
					t.Fatalf("feature state = %s, want %s", state.Value, wantState)
				}
				transitions, err := trace.Read[trace.Transition](repo, f.Stream1)
				fixturekit.Must(t, err)
				unitStates, builds := 0, 0
				for _, tr := range transitions {
					if strings.HasPrefix(tr.Subject, "unit-") {
						unitStates++
					}
					if tr.Subject == trace.FeatureSubject && tr.To == intake.BuildingState {
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
				ops, err := repo.Operations(f.Stream1)
				fixturekit.Must(t, err)
				count := 0
				for _, op := range ops {
					if op.Operation.Action == intake.BuildAction {
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
			f.Stop(t)
			f.Start(t)
			f.AwaitApproved(t)
			f.CheckContinuation(t)
			check()
		})
	}
}
