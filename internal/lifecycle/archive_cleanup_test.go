package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func captureLog(t *testing.T) (*fixturekit.SyncBuffer, *slog.Logger) {
	t.Helper()
	var buf fixturekit.SyncBuffer
	return &buf, slog.New(slog.NewJSONHandler(&buf, nil))
}

func awaitArchiveCleaned(t *testing.T, ctx context.Context, repo *trace.Repository, cleanup *journey.ArchiveCleanup, store *runtime.Store, stream config.WorkstreamID) runtime.Archive {
	t.Helper()
	var cleaned runtime.Archive
	intakefixture.Eventually(t, "archive of workstream "+string(stream)+" was never marked clean", func() bool {
		fixturekit.Must(t, cleanup.Pass(ctx))
		state, _ := store.Snapshot()
		for _, a := range state.Archived {
			if a.Workstream == stream && !a.CleanedAt.IsZero() {
				cleaned = a
				return true
			}
		}
		return false
	})
	return cleaned
}

func TestArchiveCleanupIdlePassDoesNotAnnounceActivity(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"no archives", "already cleaned", "another project"} {
		t.Run(scenario, func(t *testing.T) {
			opts, cfg := journey.ConversationFixture(t, "archive-idle-")
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: {fixturekit.Stream}}})
			fixturekit.Must(t, err)
			t.Cleanup(func() { store.Close() })
			if scenario != "no archives" {
				a := runtime.Archive{Project: fixturekit.Project, Workstream: fixturekit.Stream, Title: "Archived work", State: intake.AbandonedState, ArchivedAt: fixturekit.DemoStart}
				if scenario == "already cleaned" {
					a.CleanedAt = fixturekit.DemoStart.Add(time.Second)
				}
				fixturekit.Must(t, store.SetArchived(a))
			}
			s := &journey.Service{Cfg: cfg, Store: store, Options: opts}
			s.Activity = activity.New(s.Now)
			notifications := 0
			s.Activity.Notify = func() { notifications++ }
			if scenario == "another project" {
				cfg.Project.ID = "p_other"
			}
			cleanup := &journey.ArchiveCleanup{S: s, Workspaces: &journey.WorkspaceCleanup{Cfg: cfg, Now: s.Now}}
			for range 3 {
				fixturekit.Must(t, cleanup.Pass(context.Background()))
			}
			if notifications != 0 {
				t.Fatalf("idle cleanup announced %d activity changes", notifications)
			}
			if entries := s.Activity.View(); len(entries) != 0 {
				t.Fatalf("idle cleanup recorded activity: %+v", entries)
			}
		})
	}
}
func TestArchiveCleanupRetainsTheTitleAndDefersForABusyWorkstream(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	logged, logger := captureLog(t)
	opts, cfg := journey.ConversationFixture(t, "archive-cleanup-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	_, err = repo.Charter(ctx)
	fixturekit.Must(t, err)
	input := runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: {fixturekit.Stream, fixturekit.Quiet}}}
	store, _, err := runtime.Open(input)
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &journey.Service{Cfg: cfg, Store: store, Options: opts, Logger: logger}
	s.SetSole(journey.RuntimeFor(repo))
	h := func(kind, id string) trace.Header {
		return trace.Header{Schema: "osmia.trace." + kind, Version: trace.Version, ID: id, Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "fixture"}
	}
	_, err = repo.EnsureChiefOfStaff(ctx, fixturekit.Stream, fixturekit.DemoStart, intake.ServiceActor)
	fixturekit.Must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h("turn-request", "request-title"), AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "title", Profile: coreadapter.Profile{Name: "fake", Backend: "fake", Model: "fake"}, Prompt: "Set title"})
	fixturekit.Must(t, err)
	session := t.TempDir()
	_, err = repo.ClaimTurn(ctx, fixturekit.Stream, trace.ChiefOfStaff, "title-token", session, fixturekit.DemoStart)
	fixturekit.Must(t, err)
	_, err = repo.SetStatus(ctx, trace.ChiefOfStaff, coreadapter.Scope{Project: string(fixturekit.Project), Workstream: string(fixturekit.Stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "title"}, trace.StatusContent{Goal: "Preserved workstream title", Note: "Sensitive detail", Agents: []string{}}, fixturekit.DemoStart, func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h("document", "full-check-output"), Path: "units/u/checks-1-output.txt", Content: "sensitive stdout and stderr"}}))
	_, err = repo.SetWorkstreamBase(ctx, fixturekit.Quiet, fixturekit.Stream, 0, fixturekit.DemoStart)
	fixturekit.Must(t, err)
	_, err = repo.SetFeatureState(ctx, h("transition", "done"), intake.AbandonedState, "done")
	fixturekit.Must(t, err)
	temp := filepath.Join(cfg.Root.String(), "threads", string(fixturekit.Project), string(fixturekit.Stream))
	fixturekit.Must(t, os.MkdirAll(temp, 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(temp, "output.log"), []byte("session output"), 0600))
	before := fixturekit.TraceCommits(t, mustProjectTrace(t, cfg))
	cleanup := &journey.ArchiveCleanup{S: s, Workspaces: &journey.WorkspaceCleanup{Cfg: cfg, Repository: repo, Now: s.Now}}
	fixturekit.Must(t, cleanup.Pass(ctx))
	list, err := repo.Workstreams()
	fixturekit.Must(t, err)
	if !slices.Contains(list, fixturekit.Stream) {
		t.Fatal("terminal workstream lost before archive")
	}
	fixturekit.Must(t, store.SetArchived(runtime.Archive{Project: fixturekit.Project, Workstream: fixturekit.Stream, ArchivedAt: fixturekit.DemoStart}))
	fixturekit.Must(t, cleanup.Pass(ctx))
	raw, _ := store.Snapshot()
	if len(raw.Archived) != 1 || raw.Archived[0].Title != "Preserved workstream title" || !raw.Archived[0].CleanedAt.IsZero() {
		t.Fatalf("title not preserved before waiting: %+v", raw.Archived)
	}
	assertThere(t, temp)
	if !strings.Contains(logged.String(), fmt.Sprintf("archive cleanup of workstream %s", fixturekit.Stream)) || !strings.Contains(logged.String(), "unfinished turn") {
		t.Fatalf("deferral for a busy workstream was not logged: %s", logged.String())
	}
	fixturekit.DecodeStructuredLines(t, logged.String())
	if strings.Contains(logged.String(), "Sensitive detail") || strings.Contains(logged.String(), "sensitive stdout and stderr") {
		t.Fatalf("log carries sensitive status or document content: %s", logged.String())
	}
	fixturekit.Must(t, repo.CaptureTurn(ctx, "title-token", trace.TurnResponse{Header: h("turn-response", "response-title"), AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "title", RequestID: "request-title", RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "test-session"}, SessionDirectory: session, StartedAt: fixturekit.DemoStart, FinalResponse: "Done"}}))
	fixturekit.Must(t, repo.CompleteTurn(ctx, fixturekit.Stream, trace.ChiefOfStaff, "title", "title-token", fixturekit.DemoStart))
	fixturekit.Must(t, cleanup.Pass(ctx))
	raw, _ = store.Snapshot()
	if !raw.Archived[0].CleanedAt.IsZero() {
		t.Fatal("purged a required base")
	}
	logged.Reset()
	_, err = repo.SetWorkstreamBase(ctx, fixturekit.Quiet, "", 1, fixturekit.DemoStart)
	fixturekit.Must(t, err)
	clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, fixturekit.Stream)
	if clean.Title != "Preserved workstream title" || clean.State != intake.AbandonedState {
		t.Fatalf("archive record after cleanup: %+v", clean)
	}
	fixturekit.AssertGone(t, temp)
	list, err = repo.Workstreams()
	fixturekit.Must(t, err)
	if slices.Contains(list, fixturekit.Stream) {
		t.Fatalf("workstream trace repository not removed: %v", list)
	}
	if !slices.Contains(list, fixturekit.Quiet) {
		t.Fatalf("an unrelated workstream's trace repository was removed: %v", list)
	}
	if _, err := os.Lstat(filepath.Join(deletingDir(t, cfg), string(fixturekit.Stream))); !os.IsNotExist(err) {
		t.Fatalf("deleted trace left behind in the deletion staging area: %v", err)
	}
	if after := fixturekit.TraceCommits(t, mustProjectTrace(t, cfg)); after != before {
		t.Fatalf("archive cleanup committed to the project trace: %d commits, was %d", after, before)
	}
	fixturekit.Must(t, cleanup.Pass(ctx))
	raw, _ = store.Snapshot()
	if len(raw.Archived) != 1 || raw.Archived[0].Title != "Preserved workstream title" || raw.Archived[0].CleanedAt.IsZero() {
		t.Fatalf("repeated pass changed the archive record: %+v", raw.Archived)
	}
	if !strings.Contains(logged.String(), fmt.Sprintf("archive cleanup removed the trace of workstream %s of project %s", fixturekit.Stream, fixturekit.Project)) || !strings.Contains(logged.String(), fmt.Sprintf("archive cleanup completed for workstream %s of project %s", fixturekit.Stream, fixturekit.Project)) {
		t.Fatalf("deletion progress and completion were not logged: %s", logged.String())
	}
	fixturekit.DecodeStructuredLines(t, logged.String())
	if strings.Contains(logged.String(), "Sensitive detail") || strings.Contains(logged.String(), "sensitive stdout and stderr") {
		t.Fatalf("log carries sensitive status or document content: %s", logged.String())
	}
	got, apiFault := s.WorkstreamStatus(string(fixturekit.Stream))
	if apiFault != nil || !got.Archived || !got.TraceDeleted || got.Status == nil || got.Status.Goal != "Preserved workstream title" || got.Status.Note != "" {
		t.Fatalf("archived list: %+v %v", got, apiFault)
	}
	if _, apiFault := s.Lifecycle().Unarchive(string(fixturekit.Stream)); apiFault == nil || apiFault.Code != api.Conflict {
		t.Fatal("archive restored")
	}
}

func mustProjectTrace(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir, err := cfg.Root.ProjectTrace(cfg.Project.ID)
	fixturekit.Must(t, err)
	return dir
}

func deletingDir(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir, err := cfg.Root.WorkstreamDeleting(fixturekit.Project)
	fixturekit.Must(t, err)
	return dir
}
func TestArchiveCleanupBatchesDependenciesAndWaitsForRetainedDescendants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "archive-batch-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	descendant := config.WorkstreamID("w_00000000000000000000000000000004")
	independent := config.WorkstreamID("w_00000000000000000000000000000005")
	for _, id := range []config.WorkstreamID{descendant, independent} {
		fixturekit.Must(t, repo.CreateWorkstream(ctx, id, fixturekit.DemoStart, intake.ServiceActor))
	}
	_, err = repo.SetWorkstreamBase(ctx, fixturekit.Quiet, fixturekit.Stream, 0, fixturekit.DemoStart)
	fixturekit.Must(t, err)
	_, err = repo.SetWorkstreamBase(ctx, descendant, fixturekit.Quiet, 0, fixturekit.DemoStart)
	fixturekit.Must(t, err)
	all := []config.WorkstreamID{fixturekit.Stream, fixturekit.Quiet, descendant, independent}
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: all}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &journey.Service{Cfg: cfg, Store: store, Options: opts}
	s.SetSole(journey.RuntimeFor(repo))
	for _, id := range all {
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: fixturekit.Project, Workstream: id, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "fixture"}
		_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "done")
		fixturekit.Must(t, err)
		if id != descendant {
			fixturekit.Must(t, store.SetArchived(runtime.Archive{Project: fixturekit.Project, Workstream: id, ArchivedAt: fixturekit.DemoStart}))
		}
	}
	cleanup := &journey.ArchiveCleanup{S: s, Workspaces: &journey.WorkspaceCleanup{Cfg: cfg, Repository: repo, Now: s.Now}}
	fixturekit.Must(t, cleanup.Pass(ctx))
	state, _ := store.Snapshot()
	independentClean, streamClean, quietClean := false, false, false
	for _, a := range state.Archived {
		if a.Title == "" {
			t.Fatalf("title not preserved: %+v", a)
		}
		switch a.Workstream {
		case independent:
			independentClean = !a.CleanedAt.IsZero()
		case fixturekit.Stream:
			streamClean = !a.CleanedAt.IsZero()
		case fixturekit.Quiet:
			quietClean = !a.CleanedAt.IsZero()
		}
	}
	if streamClean || quietClean {
		t.Fatalf("cleaned a base a retained descendant still depends on: %+v", state.Archived)
	}
	if !independentClean {
		awaitArchiveCleaned(t, ctx, repo, cleanup, store, independent)
	}
	remaining, err := repo.Workstreams()
	fixturekit.Must(t, err)
	if !slices.Contains(remaining, fixturekit.Stream) || !slices.Contains(remaining, fixturekit.Quiet) {
		t.Fatalf("a base still depended on by a retained descendant was removed: %v", remaining)
	}
	if slices.Contains(remaining, independent) {
		t.Fatalf("an independent archived workstream with no base was not removed: %v", remaining)
	}
	fixturekit.Must(t, store.SetArchived(runtime.Archive{Project: fixturekit.Project, Workstream: descendant, ArchivedAt: fixturekit.DemoStart}))
	streamClean2 := awaitArchiveCleaned(t, ctx, repo, cleanup, store, fixturekit.Stream)
	if streamClean2.CleanedAt.IsZero() {
		t.Fatal("stream not cleaned once its retained descendant is archived too")
	}
	quietClean2 := awaitArchiveCleaned(t, ctx, repo, cleanup, store, fixturekit.Quiet)
	if quietClean2.CleanedAt.IsZero() {
		t.Fatal("quiet not cleaned alongside its base")
	}
	descendantClean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, descendant)
	if descendantClean.CleanedAt.IsZero() {
		t.Fatal("descendant not cleaned")
	}
	remaining, err = repo.Workstreams()
	fixturekit.Must(t, err)
	for _, id := range all {
		if slices.Contains(remaining, id) {
			t.Fatalf("workstream %s's trace repository was not removed: %v", id, remaining)
		}
	}
}

func TestArchiveCleanupDefersForAPendingOperationThenProceeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	logged, logger := captureLog(t)
	opts, cfg := journey.ConversationFixture(t, "archive-pending-op-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: {fixturekit.Stream}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &journey.Service{Cfg: cfg, Store: store, Options: opts, Logger: logger}
	s.SetSole(journey.RuntimeFor(repo))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "done")
	fixturekit.Must(t, err)
	fixturekit.RequestFactoryOperation(t, repo, fixturekit.Stream, "pending-work")
	fixturekit.Must(t, store.SetArchived(runtime.Archive{Project: fixturekit.Project, Workstream: fixturekit.Stream, ArchivedAt: fixturekit.DemoStart}))
	cleanup := &journey.ArchiveCleanup{S: s, Workspaces: &journey.WorkspaceCleanup{Cfg: cfg, Repository: repo, Now: s.Now}}
	fixturekit.Must(t, cleanup.Pass(ctx))
	raw, _ := store.Snapshot()
	if raw.Archived[0].Title == "" || !raw.Archived[0].CleanedAt.IsZero() {
		t.Fatalf("cleaned with an unresulted operation pending: %+v", raw.Archived)
	}
	if !strings.Contains(logged.String(), "operation without a result") {
		t.Fatalf("deferral for a pending operation was not logged: %s", logged.String())
	}
	fixturekit.DecodeStructuredLines(t, logged.String())
	err = repo.WithOperation(ctx, fixturekit.Stream, "pending-work", trace.Actor{Kind: "service", ID: "test"}, s.Now, func(a *trace.OperationAttempt, current trace.OperationRecord) error {
		observe := a.Action("observe", s.Now())
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "not started"}
		if err := a.Record(ctx, observe); err != nil {
			return err
		}
		if err := a.Record(ctx, a.Action("effect", s.Now())); err != nil {
			return err
		}
		result := a.Action("result", s.Now())
		result.Result = &coreadapter.OperationResult{Outcome: "done", Evidence: "test completed it"}
		if err := a.Record(ctx, result); err != nil {
			return err
		}
		return a.Record(ctx, a.Action("acknowledge", s.Now()))
	})
	fixturekit.Must(t, err)
	logged.Reset()
	clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, fixturekit.Stream)
	if clean.CleanedAt.IsZero() {
		t.Fatal("not cleaned once the operation was acknowledged")
	}
}

func TestArchiveCleanupDeletionResumesAfterInterruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "archive-interrupt-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: {fixturekit.Stream}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &journey.Service{Cfg: cfg, Store: store, Options: opts}
	s.SetSole(journey.RuntimeFor(repo))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "done")
	fixturekit.Must(t, err)
	fixturekit.Must(t, store.SetArchived(runtime.Archive{Project: fixturekit.Project, Workstream: fixturekit.Stream, ArchivedAt: fixturekit.DemoStart}))
	interrupted := false
	cleanup := &journey.ArchiveCleanup{S: s, Workspaces: &journey.WorkspaceCleanup{Cfg: cfg, Repository: repo, Now: s.Now}, Finish: func(root config.Root, p config.ProjectID, w config.WorkstreamID) error {
		if !interrupted {
			interrupted = true
			return fmt.Errorf("simulated crash mid-removal")
		}
		return trace.FinishWorkstreamTraceDeletion(root, p, w)
	}}
	fixturekit.Must(t, cleanup.Pass(ctx))
	intakefixture.Eventually(t, "trace never moved aside for deletion", func() bool {
		_, err := os.Lstat(filepath.Join(deletingDir(t, cfg), string(fixturekit.Stream)))
		return err == nil
	})
	list, err := repo.Workstreams()
	fixturekit.Must(t, err)
	if slices.Contains(list, fixturekit.Stream) {
		t.Fatal("a workstream whose trace moved aside for deletion is still listed as live")
	}
	if _, err := repo.Operations(fixturekit.Stream); err == nil {
		t.Fatal("a workstream whose trace moved aside for deletion is still readable as live")
	}
	restarted := &journey.ArchiveCleanup{S: s, Workspaces: &journey.WorkspaceCleanup{Cfg: cfg, Repository: repo, Now: s.Now}}
	clean := awaitArchiveCleaned(t, ctx, repo, restarted, store, fixturekit.Stream)
	if clean.CleanedAt.IsZero() {
		t.Fatal("interrupted deletion never completed after restart")
	}
	if _, err := os.Lstat(filepath.Join(deletingDir(t, cfg), string(fixturekit.Stream))); !os.IsNotExist(err) {
		t.Fatalf("deleted trace left behind in the deletion staging area: %v", err)
	}
}

func TestArchiveCleanupLargeDeletionDoesNotDelayReadsOrOtherWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "archive-responsive-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: {fixturekit.Stream, fixturekit.Quiet}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &journey.Service{Cfg: cfg, Store: store, Options: opts}
	s.SetSole(journey.RuntimeFor(repo))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "done")
	fixturekit.Must(t, err)
	fixturekit.Must(t, store.SetArchived(runtime.Archive{Project: fixturekit.Project, Workstream: fixturekit.Stream, ArchivedAt: fixturekit.DemoStart}))
	hold := make(chan struct{})
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	cleanup := &journey.ArchiveCleanup{S: s, Workspaces: &journey.WorkspaceCleanup{Cfg: cfg, Repository: repo, Now: s.Now}, Finish: func(root config.Root, p config.ProjectID, w config.WorkstreamID) error {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-hold:
		case <-release:
		}
		return trace.FinishWorkstreamTraceDeletion(root, p, w)
	}}
	fixturekit.Must(t, cleanup.Pass(ctx))
	select {
	case <-started:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the background removal never started")
	}
	begin := time.Now()
	if _, _, apiFault := journey.InspectForTest(s).Statuses(); apiFault != nil {
		t.Fatalf("status read failed while a deletion was in flight: %v", apiFault)
	}
	fixturekit.Must(t, repo.Append(ctx, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "note", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Quiet, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "fixture"}, Path: "handed/note.json", Content: "unrelated workstream progress"}))
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("a status read and an unrelated workstream's write waited %s on a deletion in flight", elapsed)
	}
	close(hold)
	clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, fixturekit.Stream)
	if clean.CleanedAt.IsZero() {
		t.Fatal("deletion never completed once released")
	}
}

func TestArchiveRecordsOutcomeAndPullRequestLinkThenCleansUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "archive-record-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: {fixturekit.Stream}}})
	fixturekit.Must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &journey.Service{Cfg: cfg, Store: store, Options: opts}
	s.SetSole(journey.RuntimeFor(repo))
	const pullRequestURL = "https://github.com/owner/osmia/pull/7"
	record := api.DeliveryPublication{Approval: 1, Operation: "publish-1", Status: delivery.PublicationOpened, Style: "commit-per-unit", Fork: "owner/fork", Remote: "origin", Branch: intake.FeatureBranch(fixturekit.Stream), Commit: "0123456789abcdef0123456789abcdef01234567", PullRequest: 7, URL: pullRequestURL, Title: "Ship it", Description: "a change", DescriptionHash: "hash"}
	data, err := json.Marshal(record)
	fixturekit.Must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.PublicationDocument, Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: execution.ForemanActor, Cause: "publish-1"}, Path: delivery.PublicationPath, Content: string(data)}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: execution.ForemanActor, Cause: "publish-1"}
	_, err = repo.RecordDocumentsWith(ctx, []trace.Document{doc}, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, From: "", To: intake.DeliveredState, Reason: "delivered for the test"}})
	fixturekit.Must(t, err)
	resp, apiFault := s.Lifecycle().Archive(string(fixturekit.Stream))
	if apiFault != nil || !resp.Archived {
		t.Fatalf("archive: %+v %v", resp, apiFault)
	}
	raw, _ := store.Snapshot()
	if len(raw.Archived) != 1 {
		t.Fatalf("archive record: %+v", raw.Archived)
	}
	a := raw.Archived[0]
	if a.Project != fixturekit.Project || a.Workstream != fixturekit.Stream || a.Title == "" || a.State != intake.DeliveredState || a.ArchivedAt.IsZero() || a.DeliveryLink != pullRequestURL {
		t.Fatalf("archive record: %+v", a)
	}
	cleanup := &journey.ArchiveCleanup{S: s, Workspaces: &journey.WorkspaceCleanup{Cfg: cfg, Repository: repo, Now: s.Now}}
	clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, fixturekit.Stream)
	if clean.DeliveryLink != pullRequestURL {
		t.Fatalf("delivery link lost after cleanup: %+v", clean)
	}
	got, apiFault := s.WorkstreamStatus(string(fixturekit.Stream))
	if apiFault != nil || !got.Archived || !got.TraceDeleted || got.DeliveryLink != pullRequestURL {
		t.Fatalf("archived list after cleanup: %+v %v", got, apiFault)
	}
}

func TestArchiveCleanupKeepsPendingPublicationWithoutReachingHearsay(t *testing.T) {
	t.Parallel()
	for _, publishing := range []bool{true, false} {
		t.Run(fmt.Sprintf("publishing=%v", publishing), func(t *testing.T) {
			ctx := context.Background()
			opts, cfg := journey.ConversationFixture(t, "archive-publication-")
			if publishing {
				cfg.Hearsay = config.Hearsay{URL: "https://memory.example", Principal: "owner", TokenEnv: "KEEP_OWNER", Agents: map[string]config.HearsayAgent{
					"worker": {ID: "w", TokenEnv: "KEEP_W"}, "orchestrator": {ID: "o", TokenEnv: "KEEP_O"}, "observer": {ID: "b", TokenEnv: "KEEP_B"}},
					Publish: &config.HearsayPublish{WorkSource: "osmia-work", OwnerSource: "osmia-owner", HooksURL: "http://127.0.0.1:1", Agent: "osmia-publisher", TokenEnv: "KEEP_PUBLISHER"}}
				cfg.Projects = slices.Clone(cfg.Projects)
				cfg.Projects[0] = cfg.Project
			}
			repo, err := trace.Open(cfg.Root, cfg.Project)
			fixturekit.Must(t, err)
			t.Cleanup(func() { repo.Close() })
			store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{fixturekit.Project: {fixturekit.Stream}}})
			fixturekit.Must(t, err)
			t.Cleanup(func() { store.Close() })
			s := &journey.Service{Cfg: cfg, Store: store, Options: opts}
			s.SetSole(journey.RuntimeFor(repo))
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "abandon", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: intake.ServiceActor, Cause: "fixture"}
			_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "No longer needed.")
			fixturekit.Must(t, err)
			fixturekit.Must(t, store.SetArchived(runtime.Archive{Project: fixturekit.Project, Workstream: fixturekit.Stream, ArchivedAt: fixturekit.DemoStart}))
			cleanup := &journey.ArchiveCleanup{S: s, Workspaces: &journey.WorkspaceCleanup{Cfg: cfg, Repository: repo, Now: s.Now}}
			if clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, fixturekit.Stream); clean.CleanedAt.IsZero() {
				t.Fatal("cleanup did not complete")
			}
			kept, err := repo.HearsayKept()
			fixturekit.Must(t, err)
			if !publishing {
				if len(kept) != 0 {
					t.Fatalf("kept %v without publishing configured", kept)
				}
				return
			}
			var found bool
			for _, k := range kept {
				var rec struct {
					Key      string
					Versions []struct{ State, Status, Description string }
				}
				fixturekit.Must(t, json.Unmarshal(k.Data, &rec))
				if rec.Key == "workstream/"+string(fixturekit.Stream) && !k.Released && len(rec.Versions) == 1 {
					v := rec.Versions[0]
					found = v.State == "abandoned" && v.Status == "abandoned" && strings.Contains(v.Description, "Cancelled: No longer needed.")
				}
			}
			if !found {
				t.Fatalf("the workstream's final state was not kept before its trace was removed: %v", kept)
			}
		})
	}
}
