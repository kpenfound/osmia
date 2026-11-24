package service

import (
	"context"
	"errors"
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
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestServiceDeliversEventsToTheChiefOfStaffOnceAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "ev-")
	intakefixture.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	intakefixture.Must(t, err)
	root := cfg.Root.String()

	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repo.CreateWorkstream(ctx, intakefixture.Stream, clock.Now(), owner))
	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: intakefixture.DemoAgent, Project: intakefixture.Project, Workstream: intakefixture.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: intakefixture.DemoThread}
	intakefixture.Must(t, repo.CreateThread(ctx, identity))
	for i, to := range []string{"handed", "planning"} {
		h := trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "feature_" + to, Project: intakefixture.Project, Workstream: intakefixture.Stream, At: clock.Now().Add(time.Duration(i) * time.Second), Actor: owner, Cause: "owner"}
		_, err := repo.SetFeatureState(ctx, h, to, "Owner moved the feature to "+to)
		intakefixture.Must(t, err)
	}
	intakefixture.Must(t, repo.Close())

	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "codex", ID: "session"}, FinalResponse: "Noted"}}
	fake := &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply)}
	ran := make(chan struct{}, 1)
	turns := fixturekit.TurnsFunc(func(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		select {
		case ran <- struct{}{}:
		default:
		}
		return fake.Run(ctx, p)
	})
	var bound sync.Mutex
	var live *trace.Repository
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		bound.Lock()
		live = r
		bound.Unlock()
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	pass := func(s *Service) {
		t.Helper()
		select {
		case ticks <- clock.Now():
		case <-time.After(intakefixture.DemoTimeout):
			s.Close()
			t.Fatal("service did not finish its pass")
		}
	}

	clock.Advance(14 * time.Minute)
	s, err := Start(ctx, opts)
	intakefixture.Must(t, err)
	firstService := s
	t.Cleanup(func() { _ = firstService.Close() })
	pass(s)
	pass(s)
	bound.Lock()
	repo = live
	bound.Unlock()
	th, err := repo.ChiefOfStaffThread(intakefixture.Stream)
	intakefixture.Must(t, err)
	if len(th.Turns) != 0 {
		t.Fatalf("event turn inside the window: %+v", th.Turns)
	}
	intakefixture.Must(t, s.store.SetProfile(trace.ChiefOfStaff, "other"))
	clock.Advance(2 * time.Minute)
	pass(s)
	select {
	case <-ran:
	case <-time.After(intakefixture.DemoTimeout):
		s.Close()
		t.Fatal("event turn did not run in the pass that queued it")
	}
	pass(s)
	intakefixture.Must(t, s.Close())

	s, err = Start(ctx, opts)
	intakefixture.Must(t, err)
	restartedService := s
	t.Cleanup(func() { _ = restartedService.Close() })
	clock.Advance(time.Minute)
	pass(s)
	pass(s)
	intakefixture.Must(t, s.Close())

	repo, err = trace.Open(cfg.Root, cfg.Project)
	intakefixture.Must(t, err)
	defer repo.Close()
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("backend runs %+v", calls)
	}
	call := calls[0]
	if call.Scope.Role != trace.ChiefOfStaff || !strings.HasPrefix(call.Prompt, events.Preamble) ||
		!strings.Contains(call.Prompt, "Reassess this in-flight workstream") ||
		!strings.Contains(call.Prompt, "Workstream state changed to handed: Owner moved the feature to handed") ||
		!strings.Contains(call.Prompt, "Workstream state changed from handed to planning: Owner moved the feature to planning") {
		t.Fatalf("event turn %+v", call)
	}
	th, err = repo.ChiefOfStaffThread(intakefixture.Stream)
	intakefixture.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].CompletedAt.IsZero() || th.Turns[0].Request.Profile.Name != "other" || th.Turns[0].Request.Profile.Backend != "codex" {
		t.Fatalf("chief-of-staff thread %+v", th)
	}
	worker, err := repo.Thread(intakefixture.Stream, intakefixture.DemoAgent)
	intakefixture.Must(t, err)
	if len(worker.Turns) != 0 {
		t.Fatalf("worker received a turn: %+v", worker.Turns)
	}
	entries, err := repo.Outbox(intakefixture.Stream)
	intakefixture.Must(t, err)
	notices := 0
	for _, e := range entries {
		if intakefixture.ChiefEvent(e.Event) {
			notices++
		}
		if !e.Acknowledged {
			t.Fatalf("unacknowledged %+v", e)
		}
	}
	if notices != 3 {
		t.Fatalf("outbox %+v", entries)
	}
}

func TestAbandonedWorkstreamEventsAreNotDelivered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "eva-")
	intakefixture.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	intakefixture.Must(t, err)
	root := cfg.Root.String()

	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repo.CreateWorkstream(ctx, intakefixture.Stream, clock.Now(), owner))
	for i, to := range []string{HandedState, AbandonedState} {
		h := trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: "feature_" + to, Project: intakefixture.Project, Workstream: intakefixture.Stream, At: clock.Now().Add(time.Duration(i) * time.Second), Actor: owner, Cause: "owner"}
		_, err := repo.SetFeatureState(ctx, h, to, "Owner moved the feature to "+to)
		intakefixture.Must(t, err)
	}
	intakefixture.Must(t, repo.Close())

	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: fixturekit.TurnsFunc(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
			return coreadapter.SessionResult{}, errors.New("no turn may run")
		}), Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	ticks := make(chan time.Time)
	opts.Reconciliation.Now, opts.Reconciliation.Ticks = clock.Now, ticks
	for range 2 {
		s, err := Start(ctx, opts)
		intakefixture.Must(t, err)
		clock.Advance(time.Hour)
		for range 3 {
			select {
			case ticks <- clock.Now():
			case <-time.After(intakefixture.DemoTimeout):
				s.Close()
				t.Fatal("service did not finish its pass")
			}
		}
		intakefixture.Must(t, s.Close())
	}

	repo, err = trace.Open(cfg.Root, cfg.Project)
	intakefixture.Must(t, err)
	defer repo.Close()
	th, err := repo.ChiefOfStaffThread(intakefixture.Stream)
	intakefixture.Must(t, err)
	if len(th.Turns) != 0 {
		t.Fatalf("event turns of an abandoned workstream: %+v", th.Turns)
	}
	entries, err := repo.Outbox(intakefixture.Stream)
	intakefixture.Must(t, err)
	notices := 0
	for _, e := range entries {
		if !intakefixture.ChiefEvent(e.Event) {
			continue
		}
		notices++
		if e.Acknowledged || len(e.History) != 0 {
			t.Fatalf("event of an abandoned workstream was handled: %+v", e)
		}
	}
	if notices != 2 {
		t.Fatalf("outbox %+v", entries)
	}
}
