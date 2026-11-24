package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func setUpBusyReadsService(t *testing.T, blocking coreadapter.Reconciler) (*Service, *Client, *trace.Repository) {
	t.Helper()
	opts := testOptions(fixturekit.BusyReadsConfig(t))
	opts.Reconciliation.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: blocking}
	s, c := start(t, opts)
	_, active := s.runtimeOf(fixturekit.Project)
	if active == nil {
		t.Fatal("no active project")
	}
	return s, c, active.repository
}

func readFeed(t *testing.T, c *Client, id config.WorkstreamID) FeedResponse {
	t.Helper()
	var v FeedResponse
	fixturekit.Must(t, c.Do(context.Background(), "GET", Prefix+"/feed/"+string(id), nil, &v))
	return v
}

func coreReadEndpointChecks(c *Client, workstream config.WorkstreamID) map[string]func() error {
	ctx := context.Background()
	return map[string]func() error{
		"project list (/config)":    func() error { _, err := c.Configuration(ctx); return err },
		"workstream list (/status)": func() error { _, err := c.Statuses(ctx); return err },
		"one workstream (/status/{workstream})": func() error {
			_, err := c.Status(ctx, workstream)
			return err
		},
		"workstream feed (/feed/{workstream})": func() error {
			var v FeedResponse
			return c.Do(ctx, "GET", Prefix+"/feed/"+string(workstream), nil, &v)
		},
		"conversation (/conversation/{workstream})": func() error {
			_, err := c.Conversation(ctx, workstream)
			return err
		},
		"trace summary (/trace/{workstream})": func() error {
			_, err := c.Trace(ctx, workstream)
			return err
		},
		"runtime (/runtime)":           func() error { _, err := c.Runtime(ctx); return err },
		"inbox (/inbox)":               func() error { _, err := c.Inbox(ctx); return err },
		"charter proposals (/charter)": func() error { _, err := c.CharterProposals(ctx); return err },
	}
}

func assertCoreReadEndpointsRespondWithin(t *testing.T, c *Client, workstream config.WorkstreamID, bound time.Duration) {
	t.Helper()
	for name, check := range coreReadEndpointChecks(c, workstream) {
		started := time.Now()
		if err := check(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if elapsed := time.Since(started); elapsed > bound {
			t.Errorf("%s took %s, want within %s", name, elapsed, bound)
		}
	}
	started := time.Now()
	events, _ := fixturekit.EventStream(t, c)
	fixturekit.NextEvent(t, events)
	if elapsed := time.Since(started); elapsed > bound {
		t.Errorf("event stream's first event took %s, want within %s", elapsed, bound)
	}
}

func awaitActivityEntry(t *testing.T, c *Client, match func(ActivityEntry) bool) ActivityEntry {
	t.Helper()
	var found ActivityEntry
	intakefixture.Soon(t, "a matching activity entry", func() bool {
		out, err := c.Activity(context.Background())
		fixturekit.Must(t, err)
		i := slices.IndexFunc(out.Activity, match)
		if i >= 0 {
			found = out.Activity[i]
		}
		return i >= 0
	})
	return found
}

func requestTurnOperation(t *testing.T, repository *trace.Repository, streamID config.WorkstreamID, eventID string) {
	t.Helper()
	event := trace.Event{ID: eventID, Kind: "local-effect", Body: "simulated agent turn"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(fixturekit.Project, streamID, event.ID), Boundary: coreadapter.RunnerBoundary, Action: thread.TurnAction, Input: json.RawMessage(`{}`)}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: eventID + "-start", Revision: 1, Project: fixturekit.Project, Workstream: streamID, At: fixturekit.DemoStart, Actor: serviceActor, Cause: "owner"}
	_, err := repository.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: "fake-" + eventID, To: "pending", Reason: "owner requested"}, Events: []trace.Event{event}})
	fixturekit.Must(t, err)
}

func TestReadEndpointsStayResponsiveWhileAFactoryOperationIsBlocked(t *testing.T) {
	t.Parallel()
	blocking := fixturekit.NewBlockedFactoryOperation()
	_, c, repository := setUpBusyReadsService(t, blocking)
	fixturekit.RequestFactoryOperation(t, repository, fixturekit.Stream, "busy-reads-blocked")

	select {
	case <-blocking.Started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake factory operation never started")
	}
	defer close(blocking.Release)

	assertCoreReadEndpointsRespondWithin(t, c, fixturekit.Stream, coreReadBound)
}

func TestReadEndpointsStayResponsiveWhileAFakeAgentTurnIsBlocked(t *testing.T) {
	t.Parallel()
	blocking := fixturekit.NewBlockedFactoryOperation()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, fixturekit.DemoStart, serviceActor)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.CreateWorkstream(context.Background(), fixturekit.Stream, fixturekit.DemoStart, serviceActor))
	fixturekit.Must(t, repository.Close())

	opts.Reconciliation.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: blocking}
	s, c := start(t, opts)
	_, active := s.runtimeOf(fixturekit.Project)
	if active == nil {
		t.Fatal("no active project")
	}
	requestTurnOperation(t, active.repository, fixturekit.Stream, "busy-reads-turn")

	select {
	case <-blocking.Started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake agent turn never started")
	}
	defer close(blocking.Release)

	assertCoreReadEndpointsRespondWithin(t, c, fixturekit.Stream, coreReadBound)
}

func TestReadEndpointsStayResponsiveWhileACheckRunIsBlocked(t *testing.T) {
	t.Parallel()
	checks := &fixturekit.FakeChecks{Links: []string{"dag://x"}, Outcomes: []fixturekit.CheckOutcome{{Block: true}}}
	f, _ := newMasonFixturePrepared(t, 1, independentPlan, func(opts *Options) { opts.reviewChecks = checks })
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	workstream, repository := seedBuild(t, f, "stall-check", independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repository, workstream, "resume", resumeReport)
	fixturekit.Must(t, repository.Close())
	f.start(t)
	t.Cleanup(func() { f.stop(t) })

	awaitActivityEntry(t, f.c, func(e ActivityEntry) bool { return e.Kind == ActivityCheck })

	assertCoreReadEndpointsRespondWithin(t, f.c, workstream, coreReadBound)
}

func featureTransition(t *testing.T, repository *trace.Repository, id config.WorkstreamID, to string, at time.Time) {
	t.Helper()
	state, err := repository.Workflow(id, trace.FeatureSubject)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "feature-" + to, Revision: 1, Project: fixturekit.Project, Workstream: id, At: at, Actor: serviceActor, Cause: "owner"}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, From: state.Value, To: to, Reason: "because " + to}})
	fixturekit.Must(t, err)
}

func TestWorkstreamListAndFeedShowTheNewStateAfterACommitFinishes(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, fixturekit.DemoStart, serviceActor)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.CreateWorkstream(context.Background(), fixturekit.Stream, fixturekit.DemoStart, serviceActor))
	fixturekit.Must(t, repository.Close())
	s, c := start(t, opts)
	_, active := s.runtimeOf(fixturekit.Project)
	if active == nil {
		t.Fatal("no active project")
	}
	repository = active.repository

	before, err := c.Statuses(context.Background())
	fixturekit.Must(t, err)
	if len(before.Workstreams) != 1 || before.Workstreams[0].State != nil {
		t.Fatalf("unexpected state before the factory operation finished: %+v", before.Workstreams)
	}

	featureTransition(t, repository, fixturekit.Stream, "pending", fixturekit.DemoStart.Add(time.Minute))

	after, err := c.Statuses(context.Background())
	fixturekit.Must(t, err)
	if after.Workstreams[0].State == nil || *after.Workstreams[0].State != "pending" {
		t.Fatalf("workstream list did not show the new state: %+v", after.Workstreams[0])
	}
	feed := readFeed(t, c, fixturekit.Stream)
	if !slices.ContainsFunc(feed.Entries, func(e FeedEntry) bool {
		return e.Kind == "transition" && e.Transition != nil && e.Transition.To == "pending"
	}) {
		t.Fatalf("feed did not show the new state: %+v", feed.Entries)
	}

	attempts := 0
	var raceOnce sync.Once
	s.setBoundary(func(name string) error {
		if name == "status-agents" {
			attempts++
			raceOnce.Do(func() {
				featureTransition(t, repository, fixturekit.Stream, "building", fixturekit.DemoStart.Add(2*time.Minute))
			})
		}
		return nil
	})
	defer func() { s.setBoundary(nil) }()

	raced, err := c.Statuses(context.Background())
	fixturekit.Must(t, err)
	if attempts < 2 {
		t.Fatalf("the racing commit did not force the workstream list read to retry: attempts=%d", attempts)
	}
	if raced.Workstreams[0].State == nil || *raced.Workstreams[0].State != "building" {
		t.Fatalf("workstream list returned a stale or inconsistent state after a racing commit: %+v", raced.Workstreams[0])
	}
}

func TestWorkstreamListDoesNotMixRuntimeArchiveStateAcrossGenerations(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, fixturekit.DemoStart, serviceActor)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.CreateWorkstream(context.Background(), fixturekit.Stream, fixturekit.DemoStart, serviceActor))
	fixturekit.Must(t, repository.Close())
	s, c := start(t, opts)
	_, active := s.runtimeOf(fixturekit.Project)
	if active == nil {
		t.Fatal("no active project")
	}

	attempts := 0
	var raceOnce sync.Once
	s.setBoundary(func(name string) error {
		if name == "status-agents" {
			attempts++
			raceOnce.Do(func() {
				if err := s.store.SetArchived(runtime.Archive{Project: fixturekit.Project, Workstream: fixturekit.Stream, ArchivedAt: s.now()}); err != nil {
					t.Error(err)
				}
			})
		}
		return nil
	})
	defer func() { s.setBoundary(nil) }()

	raced, err := c.Statuses(context.Background())
	fixturekit.Must(t, err)
	if attempts < 2 {
		t.Fatalf("the racing runtime mutation did not force the workstream list read to retry: attempts=%d", attempts)
	}
	idx := slices.IndexFunc(raced.Workstreams, func(w WorkstreamStatus) bool { return w.Workstream == fixturekit.Stream })
	if idx == -1 || !raced.Workstreams[idx].Archived {
		t.Fatalf("workstream list returned a stale or inconsistent archived flag after a racing runtime mutation: %+v", raced.Workstreams)
	}
}

func TestInboxDoesNotMixRuntimeStateAcrossGenerations(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, fixturekit.DemoStart, serviceActor)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.CreateWorkstream(context.Background(), fixturekit.Stream, fixturekit.DemoStart, serviceActor))
	fixturekit.Must(t, repository.Close())
	s, c := start(t, opts)
	_, active := s.runtimeOf(fixturekit.Project)
	if active == nil {
		t.Fatal("no active project")
	}

	attempts := 0
	var raceOnce sync.Once
	s.setBoundary(func(name string) error {
		if name == "inbox-decisions" {
			attempts++
			raceOnce.Do(func() {
				pause := runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: fixturekit.Stream}, Mode: "soft", Source: runtime.PauseLoopGuard, Reason: "loop guard engaged", SetAt: s.now()}
				if err := s.store.SetPause(pause); err != nil {
					t.Error(err)
				}
			})
		}
		return nil
	})
	defer func() { s.setBoundary(nil) }()

	raced, err := c.Inbox(context.Background())
	fixturekit.Must(t, err)
	if attempts < 2 {
		t.Fatalf("the racing runtime mutation did not force the inbox read to retry: attempts=%d", attempts)
	}
	if !slices.ContainsFunc(raced.Entries, func(e InboxEntry) bool { return e.Kind == InboxLoop && e.Workstream == fixturekit.Stream }) {
		t.Fatalf("inbox returned a stale or inconsistent view after a racing runtime mutation, missing the loop guard entry: %+v", raced.Entries)
	}
}

const secondStream = config.WorkstreamID("w_a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1")

func TestOwnerActionOnAnotherWorkstreamDoesNotWaitForBlockedFactoryWork(t *testing.T) {
	t.Parallel()
	blocking := fixturekit.NewBlockedFactoryOperation()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, fixturekit.DemoStart, serviceActor)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.CreateWorkstream(context.Background(), fixturekit.Stream, fixturekit.DemoStart, serviceActor))
	fixturekit.Must(t, repository.CreateWorkstream(context.Background(), secondStream, fixturekit.DemoStart, serviceActor))
	fixturekit.Must(t, repository.Close())

	opts.Reconciliation.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: blocking}
	s, c := start(t, opts)
	_, active := s.runtimeOf(fixturekit.Project)
	if active == nil {
		t.Fatal("no active project")
	}
	fixturekit.RequestFactoryOperation(t, active.repository, fixturekit.Stream, "isolation-blocked")

	select {
	case <-blocking.Started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake factory operation never started")
	}
	defer close(blocking.Release)

	ctx := context.Background()
	started := time.Now()
	if _, err := c.Abandon(ctx, secondStream, "owner action while factory work is blocked on a different workstream"); err != nil {
		t.Fatalf("owner action on another workstream: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("owner action on another workstream took %s while factory work was blocked on a different workstream", elapsed)
	}
}
