package executionjourney

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type retryPasses struct {
	clock      *fixturekit.IssueClock
	controller *reconcile.Controller
}

func newRetryPasses(t *testing.T, repository *trace.Repository, worker, action string, adapter coreadapter.Reconciler) retryPasses {
	t.Helper()
	clock := fixturekit.NewIssueClock(time.Now())
	controller, err := reconcile.New(repository, reconcile.Options{
		Worker: worker, Now: clock.Now, RetryDelay: time.Minute, MaxRetryDelay: time.Minute, GiveUpWindow: 10 * time.Minute,
		Adapters: map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: adapter},
		Hold:     func(_ config.WorkstreamID, op coreadapter.Operation) bool { return op.Action != action },
	})
	fixturekit.Must(t, err)
	return retryPasses{clock: clock, controller: controller}
}

func (p retryPasses) pass(t *testing.T, passes int) {
	t.Helper()
	for range passes {
		fixturekit.Must(t, p.controller.Pass(context.Background()))
		p.clock.Advance(time.Minute)
	}
}
