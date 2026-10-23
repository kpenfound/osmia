package executionjourney

import (
	"context"
	"errors"
	"testing"

	"github.com/kpenfound/osmia/internal/service"
)

type heldEngineControl struct {
	inside chan struct{}
	finish chan struct{}
}

func TestEngineGateReleasesCancelledCheck(t *testing.T) {
	t.Parallel()
	checks := newSlotChecks()
	f, stream, repo := newChecksFixture(t, "engine-cancel-check", checks)
	defer repo.Close()
	c, op := requestUnitCheck(t, f.s, repo, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := applyCheck(ctx, c, op)
	awaitCheck(t, checks.entered)
	control := &heldEngineControl{inside: make(chan struct{}), finish: make(chan struct{})}
	close(control.finish)
	err := f.s.RestartEngine(t.Context(), control)
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
	if err := awaitCheck(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled check: %v", err)
	}
	awaitCheckSlots(t, f.s, 0, 0)
	must(t, f.s.RestartEngine(t.Context(), control))
	awaitCheck(t, control.inside)
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
			checks := newSlotChecks()
			f, stream, repo := newChecksFixture(t, "engine-wait-"+action.name, checks)
			defer repo.Close()
			c, op := requestUnitCheck(t, f.s, repo, stream)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			control := &heldEngineControl{inside: make(chan struct{}), finish: make(chan struct{})}
			maintenance := make(chan error, 1)
			go func() { maintenance <- action.run(f.s.Service, ctx, control) }()
			awaitCheck(t, control.inside)
			finished := applyCheck(ctx, c, op)
			st := awaitCheckSlots(t, f.s, 0, 1)
			if len(st.Active) != 0 || st.Waiting[0].Unit != "resume" {
				t.Fatalf("maintenance admitted checks: %+v", st)
			}
			// Admission has reached its wait, while Docker is held. Nothing
			// durable describes a started or incomplete run in this interval.
			runs, err := checkRuns(repo, stream)
			must(t, err)
			if len(runs) != 0 || len(streamDocuments(t, repo, stream, checkSelectionDocument("resume", 1))) != 0 {
				t.Fatalf("waiting check wrote evidence: %+v", runs)
			}
			state, err := repo.Workflow(stream, checksSubject("resume"))
			must(t, err)
			if state.Value != "requested-1" {
				t.Fatalf("maintenance wait changed checks state: %+v", state)
			}
			close(control.finish)
			must(t, awaitCheck(t, maintenance))
			awaitCheck(t, checks.entered)
			checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
			must(t, awaitCheck(t, finished))
			run := onlyRun(t, repo, stream)
			if run.Status != ChecksPassed {
				t.Fatalf("wait produced %+v", run)
			}
			n, err := c.incompleteSinceEntry(stream, "resume", run, []UnitCheckRun{run})
			must(t, err)
			if n != 0 {
				t.Fatalf("maintenance wait counted as %d incomplete runs", n)
			}
			awaitCheckSlots(t, f.s, 0, 0)
		})
	}
}
