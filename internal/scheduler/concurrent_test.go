package scheduler

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/thread"
)

// Turns the reconciler runs beside its passes still stay within each role's
// capacity: with two mason slots and one reviewer slot, two mason sessions
// and one reviewer session are in flight at once and no more, and the others
// run as those complete.
func TestConcurrentTurnsStayWithinRoleCapacity(t *testing.T) {
	f, repo := setup(t)
	defer func() { repo.Close() }()
	for _, id := range []string{"mason1", "mason2", "mason3", "reviewer1", "reviewer2"} {
		f.thread(t, repo, stream, id, id[:len(id)-1])
		f.queue(t, repo, id, "one")
	}
	s, err := New(repo, Options{Now: f.clock.Now, Capacity: &config.Capacity{Masons: 2, Reviewers: 1, Committee: 1, PerWorkstream: 10}})
	must(t, err)
	var mu sync.Mutex
	running, most, ran := map[string]int{}, map[string]int{}, 0
	release := make(chan struct{})
	turns := turnsFunc(func(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		role := p.Scope.Role
		mu.Lock()
		running[role]++
		most[role] = max(most[role], running[role])
		ran++
		mu.Unlock()
		defer func() { mu.Lock(); running[role]--; mu.Unlock() }()
		select {
		case <-release:
		case <-ctx.Done():
			return coreadapter.SessionResult{}, ctx.Err()
		}
		return result(p.Scope.Turn).Value, nil
	})
	sessions := t.TempDir()
	d := thread.Dispatcher{Runner: thread.Runner{Store: repo, Turns: turns, Now: f.clock.Now},
		Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
			return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(sessions, in.Agent, in.Turn)}, nil
		}}
	ticks := make(chan time.Time)
	c, err := reconcile.New(repo, reconcile.Options{Worker: "test", Now: f.clock.Now, RetryDelay: time.Hour, Ticks: ticks, Schedule: s.Pass,
		Adapters:   map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: d},
		Concurrent: func(op coreadapter.Operation) bool { _, err := thread.DecodeTurn(op); return err == nil }})
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	await := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for {
			mu.Lock()
			met := ok()
			mu.Unlock()
			if met {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("%s: running %v", what, running)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	full := map[string]int{"mason": 2, "reviewer": 1}
	await("every slot in flight", func() bool { return maps.Equal(running, full) })
	// Two more passes while every slot is in flight start nothing more.
	ticks <- time.Now()
	ticks <- time.Now()
	mu.Lock()
	if !maps.Equal(running, full) || ran != 3 {
		t.Errorf("with every slot in flight: running %v, ran %d", running, ran)
	}
	mu.Unlock()
	close(release)
	await("every turn completed", func() bool { return ran == 5 && running["mason"]+running["reviewer"] == 0 })
	threads := func() int {
		n := 0
		for _, id := range []string{"mason1", "mason2", "mason3", "reviewer1", "reviewer2"} {
			th, err := repo.Thread(stream, id)
			if err == nil && len(th.Turns) == 1 && !th.Turns[0].CompletedAt.IsZero() {
				n++
			}
		}
		return n
	}
	await("every thread completed", func() bool { return threads() == 5 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !maps.Equal(most, full) {
		t.Fatalf("most sessions in flight at once by role %v, want %v", most, full)
	}
}
