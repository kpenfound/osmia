package lifecycle_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func assertCoreReadsStayResponsive(t *testing.T, s *journey.Service, bound time.Duration) {
	t.Helper()
	checks := map[string]func() error{
		"project list (/config)":    func() error { journey.InspectForTest(s).Configuration(); return nil },
		"workstream list (/status)": func() error { s.StatusList(); return nil },
		"inbox (/inbox)":            func() error { _, apiFault := s.Inbox(context.Background()); return apiErr(apiFault) },
		"conversation (/conversation/{workstream})": func() error {
			_, apiFault := journey.InspectForTest(s).ConversationList(string(fixturekit.Quiet))
			return apiErr(apiFault)
		},
		"workstream feed (/feed/{workstream})": func() error { _, apiFault := s.FeedList(string(fixturekit.Quiet)); return apiErr(apiFault) },
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
	opts, cfg := journey.ConversationFixture(t, "archive-stall-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	_, err = repo.Charter(ctx)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "done")
	fixturekit.Must(t, err)
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: {fixturekit.Stream, fixturekit.Quiet}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &journey.Service{Cfg: cfg, Store: store, Options: opts}
	s.Activity = activity.New(s.Now)
	s.SetSole(journey.RuntimeFor(repo))
	fixturekit.Must(t, store.SetArchived(runtime.Archive{Project: fixturekit.Project, Workstream: fixturekit.Stream, Title: "fixture workstream", State: intake.AbandonedState, ArchivedAt: fixturekit.DemoStart}))
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
	cleanup := &journey.ArchiveCleanup{S: s, Workspaces: &journey.WorkspaceCleanup{Cfg: cfg, Repository: repo, Now: s.Now}, Finish: func(root config.Root, p config.ProjectID, w config.WorkstreamID) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return trace.FinishWorkstreamTraceDeletion(root, p, w)
	}}

	fixturekit.Must(t, cleanup.Pass(ctx))
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("archive cleanup never started removing the trace")
	}

	assertCoreReadsStayResponsive(t, s, time.Second)

	activity := journey.InspectForTest(s).ActivityView()
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
	fixturekit.Must(t, err)
	if slices.Contains(remaining, fixturekit.Stream) || !slices.Contains(remaining, fixturekit.Quiet) {
		t.Fatalf("unexpected remaining workstreams: %v", remaining)
	}
}
