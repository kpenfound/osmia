package journey

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

// LifecycleCleanupGate holds the live archive cleanup at its checkpoint
// until the test releases it.
type LifecycleCleanupGate struct {
	fixturekit.Latch
	Reached   chan struct{}
	ReachOnce sync.Once
	Diagnose  func()
}

// NewLifecycleCleanupGate returns a gate that has neither been reached nor
// released.
func NewLifecycleCleanupGate() *LifecycleCleanupGate {
	return &LifecycleCleanupGate{Reached: make(chan struct{})}
}

func (g *LifecycleCleanupGate) Checkpoint(ctx context.Context, step string) error {
	return intakefixture.CleanupCheckpoint(ctx, step, g.Reached, g.Done(), &g.ReachOnce)
}

// Await waits until the live controller reaches the checkpoint. With a
// fixture it also advances that fixture's scheduled passes while waiting.
func (g *LifecycleCleanupGate) Await(t *testing.T, fixtures ...*ProductionDesignFixture) {
	t.Helper()
	if len(fixtures) == 0 {
		select {
		case <-g.Reached:
		case <-time.After(fixturekit.DemoTimeout):
			if g.Diagnose != nil {
				g.Diagnose()
			}
			t.Fatal("live controller did not reach eligible archive cleanup")
		}
		return
	}
	f := fixtures[0]
	changes, closeChanges := ServiceChanges(t, f.S)
	defer closeChanges()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		select {
		case <-g.Reached:
			return
		default:
		}
		if time.Now().After(deadline) {
			if g.Diagnose != nil {
				g.Diagnose()
			}
			t.Fatal("live controller did not reach eligible archive cleanup")
		}
		LifecycleAdvanceScheduled(t, f)
		changes()
	}
}

func LifecycleAdvanceScheduled(t *testing.T, f *ProductionDesignFixture) {
	t.Helper()
	f.SharedDesign().LifecycleAdvanceScheduled(t)
}
