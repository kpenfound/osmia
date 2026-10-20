package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

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

// TestReadEndpointsStayResponsiveWhileArchiveCleanupStallsInTraceRemoval
// stalls archive cleanup's background removal of an archived workstream's
// trace and shows that the core read endpoints about an unrelated workstream
// answer within one second, and that the removal is visible through the
// activity record while it is stalled.
func TestReadEndpointsStayResponsiveWhileArchiveCleanupStallsInTraceRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "archive-stall-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	// Record the charter now, so /config's later read of it is the fast,
	// read-only path (trace.Repository.Charter) rather than one that writes
	// the first recorded revision.
	_, err = repo.Charter(ctx)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "done")
	must(t, err)
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{project: {stream, quiet}}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts, checkSlots: &checkSlotPool{limit: cfg.Capacity.Checks}}
	s.activity = newActivityRegistry(s.now)
	s.setSole(runtimeFor(repo))
	must(t, store.SetArchived(runtime.Archive{Project: project, Workstream: stream, Title: "fixture workstream", State: AbandonedState, ArchivedAt: demoStart}))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	t.Cleanup(releaseOnce)
	cleanup := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: cfg, repository: repo, now: s.now}, finish: func(root config.Root, p config.ProjectID, w config.WorkstreamID) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return trace.FinishWorkstreamTraceDeletion(root, p, w)
	}}

	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never started removing the trace")
	}

	assertCoreReadsStayResponsive(t, s, time.Second)

	activity := s.activityView()
	idx := slices.IndexFunc(activity.Activity, func(e ActivityEntry) bool {
		return e.Kind == ActivityArchiveCleanup && e.Workstream == stream
	})
	if idx == -1 || activity.Activity[idx].Phase != "removing the workstream's trace" {
		t.Fatalf("activity did not show archive cleanup removing the trace: %+v", activity.Activity)
	}

	releaseOnce()
	clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, stream)
	if clean.CleanedAt.IsZero() {
		t.Fatal("deletion never completed once released")
	}
	remaining, err := repo.Workstreams()
	must(t, err)
	if slices.Contains(remaining, stream) || !slices.Contains(remaining, quiet) {
		t.Fatalf("unexpected remaining workstreams: %v", remaining)
	}
}
