package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// stallPurgeAt returns a trace.FailPublication hook that blocks the first
// time it is called with name until the test closes the returned release
// channel, and otherwise does nothing; the returned started channel closes
// once the stall begins.
func stallPurgeAt(name string) (hook func(string) error, started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook = func(at string) error {
		if at != name {
			return nil
		}
		once.Do(func() { close(started) })
		<-release
		return nil
	}
	return hook, started, release
}

// archiveCleanupResponsivenessFixture builds a service whose archive cleanup
// has one terminal, archived, otherwise idle workstream ready to purge, and
// a second, ordinary workstream, quiet, that stays, so a test can show that
// reads about the one that stays are not held up by cleanup of the one that
// goes. The activity record is wired up, as the running service's is, so
// cleanup's progress reports are visible the way they would be in production.
func archiveCleanupResponsivenessFixture(t *testing.T) (*Service, *trace.Repository, *archiveCleanup) {
	t.Helper()
	opts, cfg := conversationFixture(t, "archive-stall-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	// Record the charter now, so /config's later read of it is the fast,
	// read-only path (trace.Repository.Charter) rather than one that writes
	// the first recorded revision.
	_, err = repo.Charter(context.Background(), demoStart)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(context.Background(), h, AbandonedState, "done")
	must(t, err)
	input := runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{project: {stream, quiet}}}
	store, _, err := runtime.Open(input)
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts}
	s.activity = newActivityRegistry(s.now)
	s.setSole(runtimeFor(repo))
	must(t, store.SetArchived(runtime.Archive{Project: project, Workstream: stream, Title: "fixture workstream", State: AbandonedState, ArchivedAt: demoStart}))
	cleanup := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: cfg, repository: repo, now: s.now}}
	return s, repo, cleanup
}

// assertCoreReadsStayResponsive checks that status, project list, inbox,
// conversation and feed reads about an unrelated workstream answer within
// bound, by calling the same methods the HTTP handlers for those endpoints
// call.
func assertCoreReadsStayResponsive(t *testing.T, s *Service, bound time.Duration) {
	t.Helper()
	checks := map[string]func() error{
		"project list (/config)":                    func() error { s.configuration(); return nil },
		"workstream list (/status)":                 func() error { s.statusList(); return nil },
		"inbox (/inbox)":                            func() error { _, api := s.inbox(context.Background()); return apiErr(api) },
		"conversation (/conversation/{workstream})": func() error { _, api := s.conversationList(string(quiet)); return apiErr(api) },
		"workstream feed (/feed/{workstream})":      func() error { _, api := s.feedList(string(quiet)); return apiErr(api) },
	}
	for name, check := range checks {
		started := time.Now()
		if err := check(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if elapsed := time.Since(started); elapsed > bound {
			t.Errorf("%s took %s while archive cleanup was stalled", name, elapsed)
		}
	}
}

func apiErr(api *APIError) error {
	if api == nil {
		return nil
	}
	return errors.New(api.Message)
}

// runArchiveCleanupPass runs cleanup.Pass the way the reconcile controller
// does, serialized with reconciliation, and sends its result on done.
func runArchiveCleanupPass(repo *trace.Repository, cleanup *archiveCleanup, done chan<- error) {
	go func() { done <- repo.Serialize(func() error { return cleanup.Pass(context.Background()) }) }()
}

// TestReadEndpointsStayResponsiveWhileArchiveCleanupStallsInHistoryRewriting
// covers spec#1 and plan#archive-cleanup: a test stalls archive cleanup
// inside the expensive history rewrite (trace.Repository.PurgeWorkstreams,
// the "purge-rewriting" boundary, which runs before the rewrite holds any
// lock), and the core read endpoints answer within one second, and cleanup's
// progress is visible through the activity record while it is stalled.
func TestReadEndpointsStayResponsiveWhileArchiveCleanupStallsInHistoryRewriting(t *testing.T) {
	t.Parallel()
	s, repo, cleanup := archiveCleanupResponsivenessFixture(t)
	hook, started, release := stallPurgeAt("purge-rewriting")
	repo.FailPublication(hook)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	done := make(chan error, 1)
	runArchiveCleanupPass(repo, cleanup, done)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never reached history rewriting")
	}

	assertCoreReadsStayResponsive(t, s, time.Second)

	activity := s.activityView()
	idx := slices.IndexFunc(activity.Activity, func(e ActivityEntry) bool { return e.Kind == ActivityArchiveCleanup })
	if idx == -1 || activity.Activity[idx].Phase != "rewriting history" {
		t.Fatalf("activity did not show archive cleanup rewriting history: %+v", activity.Activity)
	}

	releaseOnce()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never finished")
	}
	repo.FailPublication(nil)

	remaining, err := repo.Workstreams()
	must(t, err)
	if slices.Contains(remaining, stream) || !slices.Contains(remaining, quiet) {
		t.Fatalf("unexpected remaining workstreams: %v", remaining)
	}
}

// TestReadEndpointsStayResponsiveWhileArchiveCleanupStallsInPruning covers
// spec#1 and plan#archive-cleanup: a test stalls archive cleanup inside
// pruning (trace.Repository.FinishPurge's "purge-objects-pruned" boundary,
// which runs after publication and holds neither the repository lock nor
// operationMu), and the core read endpoints still answer within one second.
func TestReadEndpointsStayResponsiveWhileArchiveCleanupStallsInPruning(t *testing.T) {
	t.Parallel()
	s, repo, cleanup := archiveCleanupResponsivenessFixture(t)
	hook, started, release := stallPurgeAt("purge-objects-pruned")
	repo.FailPublication(hook)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	done := make(chan error, 1)
	runArchiveCleanupPass(repo, cleanup, done)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never reached pruning")
	}

	assertCoreReadsStayResponsive(t, s, time.Second)

	activity := s.activityView()
	idx := slices.IndexFunc(activity.Activity, func(e ActivityEntry) bool { return e.Kind == ActivityArchiveCleanup })
	if idx == -1 || activity.Activity[idx].Phase != "pruning" {
		t.Fatalf("activity did not show archive cleanup pruning: %+v", activity.Activity)
	}

	releaseOnce()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never finished")
	}
	repo.FailPublication(nil)

	remaining, err := repo.Workstreams()
	must(t, err)
	if slices.Contains(remaining, stream) || !slices.Contains(remaining, quiet) {
		t.Fatalf("unexpected remaining workstreams: %v", remaining)
	}
}

// TestTraceWritesLandWhileArchiveCleanupPreparesItsRewrite covers spec#6 and
// spec#8: a trace write commits to an unrelated workstream while cleanup is
// preparing its rewrite (held at the "purge-rewriting" boundary, before the
// rewrite takes any lock), and cleanup's later short critical section
// carries that commit forward rather than losing it or refusing it; the
// purged workstream is absent afterward and the write's record survives.
func TestTraceWritesLandWhileArchiveCleanupPreparesItsRewrite(t *testing.T) {
	t.Parallel()
	_, repo, cleanup := archiveCleanupResponsivenessFixture(t)
	hook, started, release := stallPurgeAt("purge-rewriting")
	repo.FailPublication(hook)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	done := make(chan error, 1)
	runArchiveCleanupPass(repo, cleanup, done)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never reached history rewriting")
	}

	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "spec", Revision: 1, Project: project, Workstream: quiet, At: demoStart, Actor: serviceActor, Cause: "fixture"}, Path: "spec.md", Content: "written while cleanup prepared its rewrite\n"}
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))

	releaseOnce()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never finished")
	}
	repo.FailPublication(nil)

	remaining, err := repo.Workstreams()
	must(t, err)
	if slices.Contains(remaining, stream) || !slices.Contains(remaining, quiet) {
		t.Fatalf("unexpected remaining workstreams: %v", remaining)
	}
	docs, err := trace.Read[trace.Document](repo, quiet)
	must(t, err)
	if !slices.ContainsFunc(docs, func(d trace.Document) bool { return d.ID == "spec" && d.Path == "spec.md" }) {
		t.Fatalf("the write that landed while cleanup prepared its rewrite was lost: %+v", docs)
	}
}

// TestOwnerAnswerDuringArchiveCleanupPruningIsUnavailableThenSucceeds covers
// plan#command-admission acceptance item 1's bounded-response requirement
// against archive cleanup's own prune fence (trace.Repository.pruneMu),
// recorded as discovery owner-write-no-bound-during-prune-mutex: it stalls
// cleanup at "purge-pruning", the boundary that fires once pruneMu is
// already held (purge_test.go), and submits the owner's ruling on an
// escalated inbox entry -- one of the gated owner actions RecordDocuments
// and RecordDocumentsWith (reached here through trace.Repository.Rule)
// serve. The answer comes back Unavailable, naming the archive_cleanup
// activity entry, within a bound far short of however long the stall (or a
// real repack and prune) lasts, and records nothing. The owner's own call
// releases the repository lock as soon as its bounded wait for the fence
// gives up (lockPruneMu runs inside the single critical section Rule and
// RecordDocuments already hold for their whole call), so a background write
// started afterward, with no OwnerCommand marker, is free to reach the
// fence itself and is shown still waiting on it, unboundedly, rather than
// failing. Once the stall is released, the background write lands, cleanup
// finishes, and the same owner answer succeeds.
func TestOwnerAnswerDuringArchiveCleanupPruningIsUnavailableThenSucceeds(t *testing.T) {
	t.Parallel()
	s, repo, cleanup := archiveCleanupResponsivenessFixture(t)
	ctx := context.Background()
	home := s.cfg.Root.String()

	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: quiet, At: demoStart, Actor: serviceActor, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	_, err := repo.Ask(ctx, demoAgent, claimTurn(t, repo, home, quiet, demoAgent, demoThread, "build", demoStart), "Where does state live?", demoStart)
	must(t, err)
	_, err = repo.EscalateQuestions(ctx, trace.ChiefOfStaff, claimTurn(t, repo, home, quiet, trace.ChiefOfStaff, trace.ChiefOfStaff, "events", demoStart),
		trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "Where should state live?", Blocked: "The unit.", Recommendation: "In files."}, demoStart)
	must(t, err)

	hook, started, release := stallPurgeAt("purge-pruning")
	repo.FailPublication(hook)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	t.Cleanup(func() { repo.FailPublication(nil) })

	done := make(chan error, 1)
	runArchiveCleanupPass(repo, cleanup, done)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never reached pruning")
	}

	before, err := repo.Workflow(quiet, trace.QuestionSubject("1"))
	must(t, err)
	if before.Value != trace.QuestionEscalated {
		t.Fatalf("question before the owner's refused answer: %+v", before)
	}

	refusedAt := time.Now()
	_, api := s.answer(ctx, "1", AnswerRequest{Text: "In files."})
	if elapsed := time.Since(refusedAt); elapsed > 3*time.Second {
		t.Fatalf("owner answer during archive cleanup's prune took %s, want within bound", elapsed)
	}
	if api == nil || api.Code != Unavailable || !strings.Contains(api.Message, "archive_cleanup") {
		t.Fatalf("owner answer during archive cleanup's prune: %+v", api)
	}

	after, err := repo.Workflow(quiet, trace.QuestionSubject("1"))
	must(t, err)
	if after.Value != trace.QuestionEscalated || after.Version != before.Version {
		t.Fatalf("a refused answer changed the question: before %+v after %+v", before, after)
	}

	// A background write to the same workstream, issued once the owner's
	// own refused attempt has released the repository lock, still waits
	// for the fence rather than failing.
	backgroundDoc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "spec", Revision: 1, Project: project, Workstream: quiet, At: demoStart, Actor: serviceActor, Cause: "fixture"}, Path: "spec.md", Content: "written during the stall\n"}
	backgroundDone := make(chan error, 1)
	go func() { backgroundDone <- repo.RecordDocuments(context.Background(), []trace.Document{backgroundDoc}) }()
	select {
	case err := <-backgroundDone:
		t.Fatalf("background write was not fenced against the in-progress prune: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	releaseOnce()
	select {
	case err := <-backgroundDone:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("background write never landed once pruning finished")
	}
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never finished")
	}
	repo.FailPublication(nil)

	out, api := s.answer(ctx, "1", AnswerRequest{Text: "In files."})
	if api != nil || out.Workstream != quiet {
		t.Fatalf("owner answer after the prune finished: %+v %v", out, api)
	}
}

// TestAbandonsSecondWriteIsBoundedAndAbsorbedDuringArchiveCleanupPruning
// covers plan#command-admission review point 2 (does abandon's second trace
// write, CancelTurns, make the whole request wait out an unbounded prune
// after the critical write already landed?). abandon itself is just two
// sequential repository calls (abandon.go), so this drives them directly in
// that order with full control of when the prune starts, rather than racing
// a concurrently started prune against the first call the way the owner's
// actual HTTP request would: that race either lets the first write land
// before the fence is held (what this test needs to isolate the second
// write) or, just as validly, makes the first write itself answer
// Unavailable (already covered, for a different owner action, by
// TestOwnerAnswerDuringArchiveCleanupPruningIsUnavailableThenSucceeds).
// With the critical write (SetFeatureStateUnless) already landed before
// archive cleanup is stalled in "purge-pruning", the second write
// (CancelTurns, with a queued, unclaimed turn to cancel) answers
// ErrPruneFenceBusy within the bound rather than hanging for however long
// the prune takes, abandon.go's check absorbs that specific error rather
// than reporting a failed abandonment, and the deferred cancellation is
// exactly what the next start's recovery (cancelAbandoned) completes, the
// same path that catches up any other reason CancelTurns might fail.
func TestAbandonsSecondWriteIsBoundedAndAbsorbedDuringArchiveCleanupPruning(t *testing.T) {
	t.Parallel()
	s, repo, cleanup := archiveCleanupResponsivenessFixture(t)
	ctx := context.Background()

	identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: quiet, At: demoStart, Actor: serviceActor, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
	must(t, repo.CreateThread(ctx, identity))
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_build", Project: project, Workstream: quiet, At: demoStart, Actor: serviceActor, Cause: "fixture"},
		AgentID: demoAgent, ThreadID: demoThread, TurnID: "build", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, Prompt: "do work"})
	must(t, err)

	// The critical write lands while the fence is free, exactly like
	// abandon.go's own first call.
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: project, Workstream: quiet, At: s.now(), Actor: ownerActor, Cause: abandonTransition}
	_, err = repo.SetFeatureStateUnless(trace.OwnerCommand(ctx), h, AbandonedState, "owner abandon while archive cleanup prunes", AbandonedState, DeliveredState)
	must(t, err)
	gone, err := abandoned(repo, quiet)
	must(t, err)
	if !gone {
		t.Fatal("the critical write did not land before the prune started")
	}

	hook, started, release := stallPurgeAt("purge-pruning")
	repo.FailPublication(hook)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	t.Cleanup(func() { repo.FailPublication(nil) })

	done := make(chan error, 1)
	runArchiveCleanupPass(repo, cleanup, done)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never reached pruning")
	}

	// abandon.go's own second call: bounded by the same owner-command
	// fence, and its specific busy error is what the caller must absorb.
	startedAt := time.Now()
	_, err = repo.CancelTurns(trace.OwnerCommand(context.WithoutCancel(ctx)), quiet, s.now(), abandonActor, cancelReason)
	if elapsed := time.Since(startedAt); elapsed > 3*time.Second {
		t.Fatalf("CancelTurns during archive cleanup's prune took %s, want within bound", elapsed)
	}
	if !errors.Is(err, trace.ErrPruneFenceBusy) {
		t.Fatalf("CancelTurns during archive cleanup's prune: %v, want ErrPruneFenceBusy", err)
	}
	// abandon.go's check: err != nil && !errors.Is(err, trace.ErrPruneFenceBusy) is false here, so it
	// would return the AbandonResponse as a success, not failed.
	if err != nil && !errors.Is(err, trace.ErrPruneFenceBusy) {
		t.Fatalf("abandon.go's own check would wrongly fail this abandonment: %v", err)
	}
	// The fenced write was never applied: the queued turn's cancellation
	// is deferred rather than recorded, not silently invented as done.
	th, err := repo.Thread(quiet, demoAgent)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Response != nil {
		t.Fatalf("queued turn right after the bounded CancelTurns: %+v", th.Turns)
	}

	releaseOnce()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never finished")
	}
	repo.FailPublication(nil)

	must(t, cancelAbandoned(ctx, repo, s.now))
	th, err = repo.Thread(quiet, demoAgent)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Response == nil || !th.Turns[0].Response.Result.Cancelled {
		t.Fatalf("queued turn after recovery: %+v", th.Turns)
	}
}
