package executionjourney

import (
	context "context"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	sync "sync"
	testing "testing"
)

type lifecycleCleanupGate struct {
	reached                chan struct{}
	released               chan struct{}
	reachOnce, releaseOnce sync.Once
}

func (g *lifecycleCleanupGate) checkpoint(ctx context.Context, step string) error {
	return intakefixture.CleanupCheckpoint(ctx, step, g.reached, g.released, &g.reachOnce)
}

func (g *lifecycleCleanupGate) release() {
	g.releaseOnce.Do(func() { close(g.released) })
}

func (g *lifecycleCleanupGate) await(t *testing.T) {
	t.Helper()
	intakefixture.AwaitCleanup(t, g.reached)
}
