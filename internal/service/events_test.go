package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fixedClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fixedClock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

func TestServiceDeliversEventsToTheChiefOfStaffOnceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "ev-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	root := cfg.Root.String()

	clock := &fixedClock{now: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	for i, to := range []string{"handed", "planning"} {
		h := trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "feature_" + to, Project: project, Workstream: stream, At: clock.Now().Add(time.Duration(i) * time.Second), Actor: owner, Cause: "owner"}
		_, err := repo.SetFeatureState(ctx, h, to, "Owner moved the feature to "+to)
		must(t, err)
	}
	must(t, repo.Close())

	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "session"}, FinalResponse: "Noted"}}
	turns := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply)}
	var bound sync.Mutex
	var live *trace.Repository
	opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		bound.Lock()
		live = r
		bound.Unlock()
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	// Every tick is received only after the previous pass finished.
	pass := func(s *Service) {
		t.Helper()
		select {
		case ticks <- clock.Now():
		case <-time.After(demoTimeout):
			s.Close()
			t.Fatal("service did not finish its pass")
		}
	}

	s, err := Start(ctx, opts)
	must(t, err)
	clock.Advance(3 * time.Second)
	pass(s)
	pass(s)
	bound.Lock()
	repo = live
	bound.Unlock()
	th, err := repo.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 0 {
		t.Fatalf("event turn inside the window: %+v", th.Turns)
	}
	clock.Advance(3 * time.Second)
	pass(s)
	pass(s)
	must(t, s.Close())

	s, err = Start(ctx, opts)
	must(t, err)
	clock.Advance(time.Minute)
	pass(s)
	pass(s)
	must(t, s.Close())

	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	calls := turns.Calls()
	if len(calls) != 1 {
		t.Fatalf("backend runs %+v", calls)
	}
	call := calls[0]
	if call.Scope.Role != trace.ChiefOfStaff || !strings.HasPrefix(call.Prompt, events.Preamble) ||
		!strings.Contains(call.Prompt, "Workstream state changed to handed: Owner moved the feature to handed") ||
		!strings.Contains(call.Prompt, "Workstream state changed from handed to planning: Owner moved the feature to planning") {
		t.Fatalf("event turn %+v", call)
	}
	th, err = repo.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].CompletedAt.IsZero() || th.Turns[0].Request.Profile.Name != "default" || th.Turns[0].Request.Profile.Backend != "claude" {
		t.Fatalf("chief-of-staff thread %+v", th)
	}
	worker, err := repo.Thread(stream, demoAgent)
	must(t, err)
	if len(worker.Turns) != 0 {
		t.Fatalf("worker received a turn: %+v", worker.Turns)
	}
	entries, err := repo.Outbox(stream)
	must(t, err)
	if len(entries) != 2 {
		t.Fatalf("outbox %+v", entries)
	}
	for _, e := range entries {
		if !e.Acknowledged {
			t.Fatalf("unacknowledged %+v", e)
		}
	}
}
