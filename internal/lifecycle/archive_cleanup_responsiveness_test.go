package lifecycle_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func assertCoreReadsStayResponsive(t *testing.T, s *Service, bound time.Duration) {
	t.Helper()
	checks := map[string]func() error{
		"project list (/config)":                    func() error { s.configuration(); return nil },
		"workstream list (/status)":                 func() error { s.statusList(); return nil },
		"inbox (/inbox)":                            func() error { _, apiFault := s.inbox(context.Background()); return apiErr(apiFault) },
		"conversation (/conversation/{workstream})": func() error { _, apiFault := s.conversationList(string(fixturekit.Quiet)); return apiErr(apiFault) },
		"workstream feed (/feed/{workstream})":      func() error { _, apiFault := s.feedList(string(fixturekit.Quiet)); return apiErr(apiFault) },
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
func apiErr(apiFault *api.APIError) error {
	if apiFault == nil {
		return nil
	}
	return errors.New(apiFault.Message)
}

func TestReadEndpointsStayResponsiveWhileArchiveCleanupStallsInTraceRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "archive-stall-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	intakefixture.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	_, err = repo.Charter(ctx)
	intakefixture.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "done")
	intakefixture.Must(t, err)
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: {fixturekit.Stream, fixturekit.Quiet}}})
	intakefixture.Must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts}
	s.activity = activity.New(s.now)
	s.setSole(runtimeFor(repo))
	intakefixture.Must(t, store.SetArchived(runtime.Archive{Project: fixturekit.Project, Workstream: fixturekit.Stream, Title: "fixture workstream", State: intake.AbandonedState, ArchivedAt: fixturekit.DemoStart}))
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

	intakefixture.Must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never started removing the trace")
	}

	assertCoreReadsStayResponsive(t, s, time.Second)

	activity := s.activityView()
	idx := slices.IndexFunc(activity.Activity, func(e api.ActivityEntry) bool {
		return e.Kind == api.ActivityArchiveCleanup && e.Workstream == fixturekit.Stream
	})
	if idx == -1 || activity.Activity[idx].Phase != "removing the workstream's trace" {
		t.Fatalf("activity did not show archive cleanup removing the trace: %+v", activity.Activity)
	}

	releaseOnce()
	clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, fixturekit.Stream)
	if clean.CleanedAt.IsZero() {
		t.Fatal("deletion never completed once released")
	}
	remaining, err := repo.Workstreams()
	intakefixture.Must(t, err)
	if slices.Contains(remaining, fixturekit.Stream) || !slices.Contains(remaining, fixturekit.Quiet) {
		t.Fatalf("unexpected remaining workstreams: %v", remaining)
	}
}
