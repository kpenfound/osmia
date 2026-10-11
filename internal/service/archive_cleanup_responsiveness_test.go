package service

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
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
