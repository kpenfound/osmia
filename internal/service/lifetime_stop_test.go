package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

func TestSecondLifetimeSeesNoWorkFromTheFirst(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t)
	defer f.stop(t)
	f.stop(t)
	started, finished := make(chan struct{}), make(chan struct{})
	f.opts.Reconciliation.Schedule = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		defer close(finished)
		return ctx.Err()
	}
	f.start(t)
	select {
	case <-started:
	case <-time.After(intakefixture.DemoTimeout):
		t.Fatal("the blocking pass never started")
	}
	head := fixturekit.TraceCommits(t, f.trace)

	f.stop(t)

	select {
	case <-finished:
	default:
		t.Fatal("stop returned before the pass it started had returned")
	}
	if got := fixturekit.TraceCommits(t, f.trace); got != head {
		t.Fatalf("the trace moved after stop returned: %d commits, was %d", got, head)
	}
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repository.Close())

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
	case <-time.After(intakefixture.DemoTimeout):
		t.Fatal("the second lifetime's own pass never ran")
	}
}
