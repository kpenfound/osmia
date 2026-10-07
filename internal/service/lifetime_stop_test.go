package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

// TestSecondLifetimeSeesNoWorkFromTheFirst shows that stopping a service
// joins every goroutine it started and closes its trace and git handles
// before Stop returns, so a second lifetime started immediately afterwards
// on the same root sees no write and no goroutine left over from the first.
func TestSecondLifetimeSeesNoWorkFromTheFirst(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)
	// Restart with a pass that blocks until the service's lifetime is
	// cancelled, standing in for any of the reconcile loop's own background
	// work: a turn runner, a relay or an event stream.
	f.stop(t)
	started, finished := make(chan struct{}), make(chan struct{})
	f.opts.Reconciliation.Schedule = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		// Stop must have joined the goroutine running this very pass before
		// it returned: this only runs once the pass itself returns, and the
		// assertion right after f.stop checks that it has already run.
		defer close(finished)
		return ctx.Err()
	}
	f.start(t)
	select {
	case <-started:
	case <-time.After(demoTimeout):
		t.Fatal("the blocking pass never started")
	}
	head := strings.TrimSpace(demoGit(t, "", "-C", f.trace, "rev-parse", "HEAD"))

	f.stop(t)

	select {
	case <-finished:
	default:
		t.Fatal("stop returned before the pass it started had returned")
	}
	if got := strings.TrimSpace(demoGit(t, "", "-C", f.trace, "rev-parse", "HEAD")); got != head {
		t.Fatalf("the trace moved after stop returned: %s, was %s", got, head)
	}
	// A fresh handle on the same trace: a leftover handle from the first
	// lifetime, holding its exclusive lock, would make this fail.
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	must(t, repository.Close())

	// A second lifetime on the same root starts immediately. If the first
	// lifetime's goroutine were still running, it would race this one's own
	// pass on the same repository.
	ran := make(chan struct{}, 1)
	f.opts.Reconciliation.Schedule = func(ctx context.Context) error {
		select {
		case ran <- struct{}{}:
		default:
		}
		return nil
	}
	f.start(t)
	select {
	case <-ran:
	case <-time.After(demoTimeout):
		t.Fatal("the second lifetime's own pass never ran")
	}
}
