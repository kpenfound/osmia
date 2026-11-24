package executionjourney

import (
	"context"
	"errors"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/osmia/internal/service"
)

type heldEngineControl struct {
	inside chan struct{}
	finish chan struct{}
}

func TestEngineGateReleasesCancelledCheck(t *testing.T) {
	t.Parallel()
	checker := newSlotChecks()
	f, stream, repo := newChecksFixture(t, "engine-cancel-check", checker)
	defer repo.Close()
	c, op := requestUnitCheck(t, f.S, repo, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := intakefixture.ApplyCheck(ctx, c, op)
	intakefixture.AwaitCheck(t, checker.entered)
	control := &heldEngineControl{inside: make(chan struct{}), finish: make(chan struct{})}
	close(control.finish)
	err := f.S.RestartEngine(t.Context(), control)
	var busy *service.EngineBusyError
	if !errors.As(err, &busy) || len(busy.Checks) != 1 || busy.Checks[0].Unit != "resume" {
		t.Fatalf("running check refusal: %v", err)
	}
	select {
	case <-control.inside:
		t.Fatal("restart touched an engine with active checks")
	default:
	}
	cancel()
	if err := intakefixture.AwaitCheck(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled check: %v", err)
	}
	awaitCheckSlots(t, f.S, 0, 0)
	fixturekit.Must(t, f.S.RestartEngine(t.Context(), control))
	intakefixture.AwaitCheck(t, control.inside)
}

func (*heldEngineControl) Applicable(string) error { return nil }
func (c *heldEngineControl) Restart(ctx context.Context) error {
	close(c.inside)
	select {
	case <-c.finish:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *heldEngineControl) Prune(ctx context.Context) error { return c.Restart(ctx) }

func TestEngineMaintenanceWaitCreatesNoCheckEvidence(t *testing.T) {
	t.Parallel()
	for _, action := range []struct {
		name string
		run  func(*service.Service, context.Context, service.EngineControl) error
	}{
		{"restart", (*service.Service).RestartEngine},
		{"prune", (*service.Service).PruneEngine},
	} {
		t.Run(action.name, func(t *testing.T) {
			t.Parallel()
			checker := newSlotChecks()
			f, stream, repo := newChecksFixture(t, "engine-wait-"+action.name, checker)
			defer repo.Close()
			c, op := requestUnitCheck(t, f.S, repo, stream)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			control := &heldEngineControl{inside: make(chan struct{}), finish: make(chan struct{})}
			maintenance := make(chan error, 1)
			go func() { maintenance <- action.run(f.S.Service, ctx, control) }()
			intakefixture.AwaitCheck(t, control.inside)
			finished := intakefixture.ApplyCheck(ctx, c, op)
			st := awaitCheckSlots(t, f.S, 0, 1)
			if len(st.Active) != 0 || st.Waiting[0].Unit != "resume" {
				t.Fatalf("maintenance admitted checks: %+v", st)
			}

			runs, err := execution.CheckRuns(repo, stream)
			fixturekit.Must(t, err)
			if len(runs) != 0 || len(intakefixture.StreamDocuments(t, repo, stream, execution.CheckSelectionDocument("resume", 1))) != 0 {
				t.Fatalf("waiting check wrote evidence: %+v", runs)
			}
			state, err := repo.Workflow(stream, execution.ChecksSubject("resume"))
			fixturekit.Must(t, err)
			if state.Value != "requested-1" {
				t.Fatalf("maintenance wait changed checks state: %+v", state)
			}
			close(control.finish)
			fixturekit.Must(t, intakefixture.AwaitCheck(t, maintenance))
			intakefixture.AwaitCheck(t, checker.entered)
			checker.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
			fixturekit.Must(t, intakefixture.AwaitCheck(t, finished))
			run := onlyRun(t, repo, stream)
			if run.Status != checks.ChecksPassed {
				t.Fatalf("wait produced %+v", run)
			}
			n, err := c.Driver().IncompleteSinceEntry(stream, "resume", run, []execution.UnitCheckRun{run})
			fixturekit.Must(t, err)
			if n != 0 {
				t.Fatalf("maintenance wait counted as %d incomplete runs", n)
			}
			awaitCheckSlots(t, f.S, 0, 0)
		})
	}
}
