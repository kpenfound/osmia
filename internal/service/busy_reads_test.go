package service

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// blockedFactoryOperation is a fake RepositoryBoundary reconciler whose Apply
// blocks until release closes. A RepositoryBoundary operation is never in
// concurrentOperation (service.go, concurrentOperation), so the reconcile
// controller runs it the same way it runs a real rebase, merge, build, seal
// or publish: synchronously inside trace.Repository.WithOperation, which
// holds the repository's operationMu for the whole call rather than running
// it through trace.OperationAttempt.Unlocked (internal/reconcile/
// controller.go, Controller.reconcile; internal/trace/operations.go,
// WithOperation). This fake stands in for any of those without needing a
// real workspace or build. operationMu by itself does not block the reads
// this test checks: WithOperation releases r.mu before calling fn
// (internal/trace/operations.go, WithOperation), and the read endpoints
// (Workstreams, Operations, Read, Threads, Statuses) never acquire
// operationMu. What this test guards against is the repository's other
// lock, r.mu, which every read and every write shares: before this unit's
// fix, every call that needed it, including the handful of bookkeeping
// writes WithOperation itself makes around a blocked effect (claim,
// observe, effect-start), re-walked and re-validated the whole committed
// trace against Git while holding it (internal/trace/git.go, checkHistory;
// internal/trace/repository.go, scanTrace), so reads queued behind whatever
// write currently held it, worse the larger the trace. Repository.Generation
// and the generation-cached scan and history (internal/trace/repository.go)
// let a read reuse the last scan when nothing has changed instead of paying
// that cost again.
type blockedFactoryOperation struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockedFactoryOperation() *blockedFactoryOperation {
	return &blockedFactoryOperation{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockedFactoryOperation) Inspect(context.Context, coreadapter.Operation) (coreadapter.Observation, error) {
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "fake factory operation in progress"}, nil
}

func (b *blockedFactoryOperation) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return coreadapter.OperationResult{}, ctx.Err()
	}
	return coreadapter.OperationResult{Outcome: "complete", Evidence: "fake factory operation finished"}, nil
}

// requestFactoryOperation durably records a RepositoryBoundary operation of
// streamID, under an action the service does not reserve for sealing,
// building, landing, rebasing, drift or publishing, so the reconcile
// controller routes it to the test's own fake adapter (sealing.go,
// repositoryAdapter.Apply) rather than to a real one.
func requestFactoryOperation(t *testing.T, repository *trace.Repository, streamID config.WorkstreamID, eventID string) {
	t.Helper()
	event := trace.Event{ID: eventID, Kind: "local-effect", Body: "simulated factory work"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(project, streamID, event.ID), Boundary: coreadapter.RepositoryBoundary, Action: "fake-factory-op", Input: json.RawMessage(`{}`)}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: eventID + "-start", Revision: 1, Project: project, Workstream: streamID, At: demoStart, Actor: serviceActor, Cause: "owner"}
	_, err := repository.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: "fake-" + eventID, To: "pending", Reason: "owner requested"}, Events: []trace.Event{event}})
	must(t, err)
}

// setUpBusyReadsService starts a service whose only active project has
// stream already created, wired so a RepositoryBoundary operation reaches
// blocking instead of a real sealer, builder or foreman.
func setUpBusyReadsService(t *testing.T, blocking coreadapter.Reconciler) (*Service, *Client, *trace.Repository) {
	t.Helper()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, demoStart, serviceActor)
	must(t, err)
	must(t, repository.CreateWorkstream(context.Background(), stream, demoStart, serviceActor))
	must(t, repository.Close())

	opts.Reconciliation.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: blocking}
	s, c := start(t, opts)
	_, active := s.runtimeOf(project)
	if active == nil {
		t.Fatal("no active project")
	}
	return s, c, active.repository
}

// readFeed fetches a workstream's feed through the same HTTP path the web UI
// uses on refresh.
func readFeed(t *testing.T, c *Client, id config.WorkstreamID) FeedResponse {
	t.Helper()
	var v FeedResponse
	must(t, c.Do(context.Background(), "GET", Prefix+"/feed/"+string(id), nil, &v))
	return v
}

// TestReadEndpointsStayResponsiveWhileAFactoryOperationIsBlocked covers
// spec#1: the read endpoints the web UI calls on first load and on feed
// refresh answer within one second while a factory operation that does not
// call trace.OperationAttempt.Unlocked, such as a rebase, merge, build, seal
// or publish, is still in progress.
func TestReadEndpointsStayResponsiveWhileAFactoryOperationIsBlocked(t *testing.T) {
	t.Parallel()
	blocking := newBlockedFactoryOperation()
	_, c, repository := setUpBusyReadsService(t, blocking)
	requestFactoryOperation(t, repository, stream, "busy-reads-blocked")

	select {
	case <-blocking.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake factory operation never started")
	}
	defer close(blocking.release)

	ctx := context.Background()
	// The web UI's stream resync reads status, runtime, config, inbox,
	// charter proposals and every listed workstream's feed (internal/web/
	// app.js, the "reads" map's resync entry), so every one of them must
	// answer promptly while a factory operation is in progress.
	checks := map[string]func() error{
		"project list (/config)":    func() error { _, err := c.Configuration(ctx); return err },
		"workstream list (/status)": func() error { _, err := c.Statuses(ctx); return err },
		"workstream feed (/feed/{workstream})": func() error {
			var v FeedResponse
			return c.Do(ctx, "GET", Prefix+"/feed/"+string(stream), nil, &v)
		},
		"runtime (/runtime)":           func() error { _, err := c.Runtime(ctx); return err },
		"inbox (/inbox)":               func() error { _, err := c.Inbox(ctx); return err },
		"charter proposals (/charter)": func() error { _, err := c.CharterProposals(ctx); return err },
	}
	for name, check := range checks {
		started := time.Now()
		if err := check(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Errorf("%s took %s while a factory operation was blocked", name, elapsed)
		}
	}
}

// featureTransition commits a transition of stream's feature subject to to,
// as a factory operation finishing would, and returns once it is durable.
func featureTransition(t *testing.T, repository *trace.Repository, id config.WorkstreamID, to string, at time.Time) {
	t.Helper()
	state, err := repository.Workflow(id, trace.FeatureSubject)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "feature-" + to, Revision: 1, Project: project, Workstream: id, At: at, Actor: serviceActor, Cause: "owner"}
	_, err = repository.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, From: state.Value, To: to, Reason: "because " + to}})
	must(t, err)
}

// TestWorkstreamListAndFeedShowTheNewStateAfterACommitFinishes covers
// spec#2: once a factory operation finishes changing a workstream's state,
// the next workstream list and feed reads show the new state; and a
// workstream list read that races a commit is retried whole (status.go,
// consistentRead) rather than returning a response assembled partly from
// before the commit and partly from after it.
func TestWorkstreamListAndFeedShowTheNewStateAfterACommitFinishes(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, demoStart, serviceActor)
	must(t, err)
	must(t, repository.CreateWorkstream(context.Background(), stream, demoStart, serviceActor))
	must(t, repository.Close())
	s, c := start(t, opts)
	_, active := s.runtimeOf(project)
	if active == nil {
		t.Fatal("no active project")
	}
	repository = active.repository

	before, err := c.Statuses(context.Background())
	must(t, err)
	if len(before.Workstreams) != 1 || before.Workstreams[0].State != nil {
		t.Fatalf("unexpected state before the factory operation finished: %+v", before.Workstreams)
	}

	// The factory operation finishes and changes the workstream's state.
	featureTransition(t, repository, stream, "pending", demoStart.Add(time.Minute))

	after, err := c.Statuses(context.Background())
	must(t, err)
	if after.Workstreams[0].State == nil || *after.Workstreams[0].State != "pending" {
		t.Fatalf("workstream list did not show the new state: %+v", after.Workstreams[0])
	}
	feed := readFeed(t, c, stream)
	if !slices.ContainsFunc(feed.Entries, func(e FeedEntry) bool {
		return e.Kind == "transition" && e.Transition != nil && e.Transition.To == "pending"
	}) {
		t.Fatalf("feed did not show the new state: %+v", feed.Entries)
	}

	// A commit that lands while a workstream list read is assembling its
	// response is retried from scratch (status.go, Service.statuses, through
	// consistentRead), so the read never answers with some of its fields
	// from before the commit and some from after it.
	attempts := 0
	var raceOnce sync.Once
	s.boundary = func(name string) error {
		if name == "status-agents" {
			attempts++
			raceOnce.Do(func() { featureTransition(t, repository, stream, "building", demoStart.Add(2*time.Minute)) })
		}
		return nil
	}
	defer func() { s.boundary = nil }()

	raced, err := c.Statuses(context.Background())
	must(t, err)
	if attempts < 2 {
		t.Fatalf("the racing commit did not force the workstream list read to retry: attempts=%d", attempts)
	}
	if raced.Workstreams[0].State == nil || *raced.Workstreams[0].State != "building" {
		t.Fatalf("workstream list returned a stale or inconsistent state after a racing commit: %+v", raced.Workstreams[0])
	}
}

// TestConsistentReadRetriesAcrossARacingCommit shows the mechanism behind
// the fix directly: a read that performs several separate Repository calls
// is retried whole when a commit lands between them, so it never returns a
// result assembled from two different generations of the trace.
func TestConsistentReadRetriesAcrossARacingCommit(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, demoStart, serviceActor)
	must(t, err)
	t.Cleanup(func() { repository.Close() })
	must(t, repository.CreateWorkstream(context.Background(), stream, demoStart, serviceActor))

	const racingStream = config.WorkstreamID("w_1111111111111111111111111111aaaa")
	raced := false
	attempts := 0
	type snapshot struct{ before, after int }
	result, err := consistentRead(repository, func() (snapshot, error) {
		attempts++
		before, err := repository.Workstreams()
		if err != nil {
			return snapshot{}, err
		}
		if !raced {
			raced = true
			// Committed from inside fn, between its two separate Repository
			// calls, so an unprotected caller would see a different
			// workstream count on each.
			if err := repository.CreateWorkstream(context.Background(), racingStream, demoStart, serviceActor); err != nil {
				return snapshot{}, err
			}
		}
		after, err := repository.Workstreams()
		if err != nil {
			return snapshot{}, err
		}
		return snapshot{len(before), len(after)}, nil
	})
	must(t, err)
	if attempts < 2 {
		t.Fatalf("the racing commit did not force a retry: attempts=%d", attempts)
	}
	if result.before != result.after {
		t.Fatalf("consistentRead returned a torn result: %+v", result)
	}
	if result.before != 2 {
		t.Fatalf("consistentRead did not settle on the latest generation: %+v", result)
	}
}

// TestConsistentReadReturnsWithoutARacingWrite shows the common case: with
// nothing racing it, consistentRead calls fn exactly once.
func TestConsistentReadReturnsWithoutARacingWrite(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, demoStart, serviceActor)
	must(t, err)
	t.Cleanup(func() { repository.Close() })

	attempts := 0
	_, err = consistentRead(repository, func() (uint64, error) {
		attempts++
		return repository.Generation(), nil
	})
	must(t, err)
	if attempts != 1 {
		t.Fatalf("fn ran %d times with nothing racing it", attempts)
	}
}
