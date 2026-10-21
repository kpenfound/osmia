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
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// blockedFactoryOperation holds a repository effect until release closes.
// Its unreserved action runs inline so read endpoints can be exercised while
// the scheduling pass is occupied. Reads use the trace's short bookkeeping
// lock and generation cache; they must not wait for the effect to finish.
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

// coreReadEndpointChecks lists, by name, a bounded check of every core read
// endpoint named in plan#read-boundaries, plus the project list and charter
// proposals the web UI's own resync reads alongside them (internal/web/
// app.js, the "reads" map's resync entry), since those share the same
// trace and runtime locks.
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

// assertCoreReadEndpointsRespondWithin checks that every endpoint
// coreReadEndpointChecks names, and the event stream's first event, answers
// within bound.
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
	events, _ := eventStream(t, c)
	nextEvent(t, events)
	if elapsed := time.Since(started); elapsed > bound {
		t.Errorf("event stream's first event took %s, want within %s", elapsed, bound)
	}
}

// awaitActivityEntry polls GET /activity until an entry matches, or fails
// the test after 10s.
func awaitActivityEntry(t *testing.T, c *Client, match func(ActivityEntry) bool) ActivityEntry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := c.Activity(context.Background())
		must(t, err)
		if i := slices.IndexFunc(out.Activity, match); i >= 0 {
			return out.Activity[i]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching activity entry within 10s: %+v", out.Activity)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requestTurnOperation durably records a RunnerBoundary operation of
// streamID with the real thread.TurnAction, so the reconcile controller
// routes it to the test's own fake turns adapter exactly as it would a real
// queued agent turn (service.go, concurrentOperation, runnerAdapter), and
// runs it concurrently with the rest of the pass through
// trace.OperationAttempt.Unlocked.
func requestTurnOperation(t *testing.T, repository *trace.Repository, streamID config.WorkstreamID, eventID string) {
	t.Helper()
	event := trace.Event{ID: eventID, Kind: "local-effect", Body: "simulated agent turn"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(project, streamID, event.ID), Boundary: coreadapter.RunnerBoundary, Action: thread.TurnAction, Input: json.RawMessage(`{}`)}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: eventID + "-start", Revision: 1, Project: project, Workstream: streamID, At: demoStart, Actor: serviceActor, Cause: "owner"}
	_, err := repository.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: "fake-" + eventID, To: "pending", Reason: "owner requested"}, Events: []trace.Event{event}})
	must(t, err)
}

// TestReadEndpointsStayResponsiveWhileAFactoryOperationIsBlocked covers
// spec#1: the read endpoints the web UI calls on first load and on feed
// refresh answer within the latency bound while a Git or workspace
// operation that does not call trace.OperationAttempt.Unlocked, such as a
// rebase, merge, build, seal or publish, is still in progress.
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

	assertCoreReadEndpointsRespondWithin(t, c, stream, coreReadBound)
}

// TestReadEndpointsStayResponsiveWhileAFakeAgentTurnIsBlocked covers
// spec#1: a queued thread turn, a RunnerBoundary operation the reconcile
// controller runs beside the rest of its pass through
// trace.OperationAttempt.Unlocked (service.go, concurrentOperation), still
// lets every core read endpoint answer within the latency bound while it is
// blocked.
func TestReadEndpointsStayResponsiveWhileAFakeAgentTurnIsBlocked(t *testing.T) {
	t.Parallel()
	blocking := newBlockedFactoryOperation()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, demoStart, serviceActor)
	must(t, err)
	must(t, repository.CreateWorkstream(context.Background(), stream, demoStart, serviceActor))
	must(t, repository.Close())

	opts.Reconciliation.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RunnerBoundary: blocking}
	s, c := start(t, opts)
	_, active := s.runtimeOf(project)
	if active == nil {
		t.Fatal("no active project")
	}
	requestTurnOperation(t, active.repository, stream, "busy-reads-turn")

	select {
	case <-blocking.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake agent turn never started")
	}
	defer close(blocking.release)

	assertCoreReadEndpointsRespondWithin(t, c, stream, coreReadBound)
}

// TestReadEndpointsStayResponsiveWhileACheckRunIsBlocked covers spec#1: a
// unit's checks, a RunnerBoundary operation with the real CheckAction that
// the reconcile controller also runs concurrently (service.go,
// concurrentOperation; unit_checks.go, checkers.Apply), running the real
// checks pipeline against a fake check runner that blocks the way an
// unresponsive dagger check would, still lets every core read endpoint
// answer within the latency bound while it is blocked.
func TestReadEndpointsStayResponsiveWhileACheckRunIsBlocked(t *testing.T) {
	t.Parallel()
	checks := &fakeChecks{Links: []string{"dag://x"}, Outcomes: []checkOutcome{{Block: true}}}
	f, _ := newMasonFixturePrepared(t, 1, independentPlan, func(opts *Options) { opts.reviewChecks = checks })
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	workstream, repository := seedBuild(t, f, "stall-check", independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repository, workstream, "resume", resumeReport)
	must(t, repository.Close())
	// Restart with a live reconcile loop: seedBuild and seedChecking work
	// against a stopped service's own repository handle, which must be
	// closed before the service can reopen it.
	f.start(t)
	t.Cleanup(func() { f.stop(t) })

	awaitActivityEntry(t, f.c, func(e ActivityEntry) bool { return e.Kind == ActivityCheck })

	assertCoreReadEndpointsRespondWithin(t, f.c, workstream, coreReadBound)
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
	result, err := consistentRead(func() (snapshot, error) {
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
	}, repository.Generation)
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

// TestWorkstreamListDoesNotMixRuntimeArchiveStateAcrossGenerations covers
// spec#8's runtime half: statuses (status.go) reads the runtime store and
// the trace together, inside one consistentRead attempt scoped to both the
// repository's generation and the runtime store's own (runtime/store.go,
// Store.Generation). A runtime mutation that lands while that attempt is
// assembling its response, here archiving the workstream, forces the whole
// attempt to retry, so the response never shows a workstream's archived
// flag from one runtime generation alongside the rest of its fields from
// another.
func TestWorkstreamListDoesNotMixRuntimeArchiveStateAcrossGenerations(t *testing.T) {
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

	attempts := 0
	var raceOnce sync.Once
	s.boundary = func(name string) error {
		if name == "status-agents" {
			attempts++
			raceOnce.Do(func() {
				if err := s.store.SetArchived(runtime.Archive{Project: project, Workstream: stream, ArchivedAt: s.now()}); err != nil {
					t.Error(err)
				}
			})
		}
		return nil
	}
	defer func() { s.boundary = nil }()

	raced, err := c.Statuses(context.Background())
	must(t, err)
	if attempts < 2 {
		t.Fatalf("the racing runtime mutation did not force the workstream list read to retry: attempts=%d", attempts)
	}
	idx := slices.IndexFunc(raced.Workstreams, func(w WorkstreamStatus) bool { return w.Workstream == stream })
	if idx == -1 || !raced.Workstreams[idx].Archived {
		t.Fatalf("workstream list returned a stale or inconsistent archived flag after a racing runtime mutation: %+v", raced.Workstreams)
	}
}

// TestInboxDoesNotMixRuntimeStateAcrossGenerations covers spec#8: inbox
// (inbox.go) reads a project's statuses, escalations and, per workstream,
// many further trace and runtime-store facts (operations, outbox, the loop
// guard's pause, ...), inside one consistentRead attempt scoped to both the
// repository's and the runtime store's generation. A runtime mutation that
// lands partway through that attempt, here the owner's loop guard pausing
// the workstream, forces the whole attempt to retry rather than return a
// response built from entries read across two different runtime
// generations: the earlier entries this attempt already assembled, from
// before the pause, and the loop guard's own entry, which only a fresh
// attempt would see.
func TestInboxDoesNotMixRuntimeStateAcrossGenerations(t *testing.T) {
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

	attempts := 0
	var raceOnce sync.Once
	s.boundary = func(name string) error {
		if name == "inbox-decisions" {
			attempts++
			raceOnce.Do(func() {
				pause := runtime.Pause{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: stream}, Mode: "soft", Source: runtime.PauseLoopGuard, Reason: "loop guard engaged", SetAt: s.now()}
				if err := s.store.SetPause(pause); err != nil {
					t.Error(err)
				}
			})
		}
		return nil
	}
	defer func() { s.boundary = nil }()

	raced, err := c.Inbox(context.Background())
	must(t, err)
	if attempts < 2 {
		t.Fatalf("the racing runtime mutation did not force the inbox read to retry: attempts=%d", attempts)
	}
	if !slices.ContainsFunc(raced.Entries, func(e InboxEntry) bool { return e.Kind == InboxLoop && e.Workstream == stream }) {
		t.Fatalf("inbox returned a stale or inconsistent view after a racing runtime mutation, missing the loop guard entry: %+v", raced.Entries)
	}
}

// secondStream is a second workstream in the same repository and project as
// stream, used to show that a factory operation blocked on one workstream
// does not hold up an owner action on another.
const secondStream = config.WorkstreamID("w_a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1")

// TestOwnerActionOnAnotherWorkstreamDoesNotWaitForBlockedFactoryWork covers
// spec#3 and plan#api-action-isolation: an owner action submitted for one
// workstream is accepted or refused without waiting for factory work in
// progress on a different workstream of the same project, even when that
// work is a RepositoryBoundary operation such as a rebase, merge, build,
// seal or publish. Repository workflow effects run beside the scheduling
// pass under their own workstream's ordering.
//
// The workflow attempt reserves its own workstream, and its claim and result
// writes take only the brief trace bookkeeping lock. An owner action on
// another workstream takes that workstream's bookkeeping lock and never waits
// for this workflow effect.
func TestOwnerActionOnAnotherWorkstreamDoesNotWaitForBlockedFactoryWork(t *testing.T) {
	t.Parallel()
	blocking := newBlockedFactoryOperation()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, demoStart, serviceActor)
	must(t, err)
	must(t, repository.CreateWorkstream(context.Background(), stream, demoStart, serviceActor))
	must(t, repository.CreateWorkstream(context.Background(), secondStream, demoStart, serviceActor))
	must(t, repository.Close())

	opts.Reconciliation.Adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{coreadapter.RepositoryBoundary: blocking}
	s, c := start(t, opts)
	_, active := s.runtimeOf(project)
	if active == nil {
		t.Fatal("no active project")
	}
	requestFactoryOperation(t, active.repository, stream, "isolation-blocked")

	select {
	case <-blocking.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fake factory operation never started")
	}
	defer close(blocking.release)

	ctx := context.Background()
	started := time.Now()
	if _, err := c.Abandon(ctx, secondStream, "owner action while factory work is blocked on a different workstream"); err != nil {
		t.Fatalf("owner action on another workstream: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("owner action on another workstream took %s while factory work was blocked on a different workstream", elapsed)
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
	_, err = consistentRead(func() (uint64, error) {
		attempts++
		return repository.Generation(), nil
	}, repository.Generation)
	must(t, err)
	if attempts != 1 {
		t.Fatalf("fn ran %d times with nothing racing it", attempts)
	}
}
