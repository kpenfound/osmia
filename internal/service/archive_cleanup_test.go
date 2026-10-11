package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestArchiveCleanupMigratesOldArchivesAndRetainsTitle(t *testing.T) {
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "archive-cleanup-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	_, err = repo.Charter(ctx, demoStart)
	must(t, err)
	input := runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{project: {stream, quiet}}}
	store, _, err := runtime.Open(input)
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts}
	s.setSole(runtimeFor(repo))
	h := func(kind, id string) trace.Header {
		return trace.Header{Schema: "osmia.trace." + kind, Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "fixture"}
	}
	_, err = repo.EnsureChiefOfStaff(ctx, stream, demoStart, serviceActor)
	must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h("turn-request", "request-title"), AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "title", Profile: coreadapter.Profile{Name: "fake", Backend: "fake", Model: "fake"}, Prompt: "Set title"})
	must(t, err)
	session := t.TempDir()
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "title-token", session, demoStart)
	must(t, err)
	_, err = repo.SetStatus(ctx, trace.ChiefOfStaff, coreadapter.Scope{Project: string(project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "title"}, trace.StatusContent{Goal: "Preserved workstream title", Note: "Sensitive detail", Agents: []string{}}, demoStart, func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	must(t, err)
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h("document", "full-check-output"), Path: "units/u/checks-1-output.txt", Content: "sensitive stdout and stderr"}}))
	_, err = repo.SetWorkstreamBase(ctx, quiet, stream, 0, demoStart)
	must(t, err)
	_, err = repo.SetFeatureState(ctx, h("transition", "done"), AbandonedState, "done")
	must(t, err)
	temp := filepath.Join(cfg.Root.String(), "threads", string(project), string(stream))
	must(t, os.MkdirAll(temp, 0700))
	must(t, os.WriteFile(filepath.Join(temp, "output.log"), []byte("session output"), 0600))
	cleanup := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: cfg, repository: repo, now: s.now}}
	// Terminal state alone keeps the durable trace, even after workspace cleanup.
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	list, err := repo.Workstreams()
	must(t, err)
	if !slices.Contains(list, stream) {
		t.Fatal("terminal workstream lost before archive")
	}
	// An archive without metadata is the persisted shape requiring migration.
	must(t, store.SetArchived(runtime.Archive{Project: project, Workstream: stream, ArchivedAt: demoStart}))
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	raw, _ := store.Snapshot()
	if len(raw.Archived) != 1 || raw.Archived[0].Title != "Preserved workstream title" || !raw.Archived[0].CleanedAt.IsZero() {
		t.Fatalf("title not preserved before waiting: %+v", raw.Archived)
	}
	assertThere(t, temp)
	// The existing turn must finish before any trace or session output goes.
	must(t, repo.CaptureTurn(ctx, "title-token", trace.TurnResponse{Header: h("turn-response", "response-title"), AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "title", RequestID: "request-title", RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "test-session"}, SessionDirectory: session, StartedAt: demoStart, FinalResponse: "Done"}}))
	must(t, repo.CompleteTurn(ctx, stream, trace.ChiefOfStaff, "title", "title-token", demoStart))
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	raw, _ = store.Snapshot()
	if !raw.Archived[0].CleanedAt.IsZero() {
		t.Fatal("purged a required base")
	}
	_, err = repo.SetWorkstreamBase(ctx, quiet, "", 1, demoStart)
	must(t, err)
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	raw, _ = store.Snapshot()
	// Deleting the workstream's own trace repository belongs to
	// archive-independent-deletion. With every deferral condition clear,
	// cleanup stays ready but does not mark the archive clean, and the
	// trace repository is untouched (spec#6, spec#8).
	if !raw.Archived[0].CleanedAt.IsZero() {
		t.Fatal("archive cleanup marked clean before its trace repository is gone")
	}
	assertGone(t, temp)
	list, err = repo.Workstreams()
	must(t, err)
	if !slices.Contains(list, stream) || !slices.Contains(list, quiet) {
		t.Fatalf("workstream trace repository removed before archive-independent-deletion: %v", list)
	}
	// A repeated pass stays stable: it neither completes nor regresses what
	// already cleared.
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	raw, _ = store.Snapshot()
	if len(raw.Archived) != 1 || raw.Archived[0].Title != "Preserved workstream title" || !raw.Archived[0].CleanedAt.IsZero() {
		t.Fatalf("repeated pass changed the archive record: %+v", raw.Archived)
	}
	got, api := s.workstreamStatus(string(stream))
	if api != nil || !got.Archived || got.TraceDeleted || got.Status == nil || got.Status.Goal != "Preserved workstream title" || got.Status.Note != "" {
		t.Fatalf("archived list: %+v %v", got, api)
	}
	if _, api := s.unarchive(string(stream)); api == nil || api.Code != Conflict {
		t.Fatal("archive restored")
	}
}

func TestArchiveCleanupBatchesDependenciesAndWaitsForRetainedDescendants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "archive-batch-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	descendant := config.WorkstreamID("w_00000000000000000000000000000004")
	independent := config.WorkstreamID("w_00000000000000000000000000000005")
	for _, id := range []config.WorkstreamID{descendant, independent} {
		must(t, repo.CreateWorkstream(ctx, id, demoStart, serviceActor))
	}
	_, err = repo.SetWorkstreamBase(ctx, quiet, stream, 0, demoStart)
	must(t, err)
	_, err = repo.SetWorkstreamBase(ctx, descendant, quiet, 0, demoStart)
	must(t, err)
	all := []config.WorkstreamID{stream, quiet, descendant, independent}
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{project: all}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts}
	s.setSole(runtimeFor(repo))
	for _, id := range all {
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: project, Workstream: id, At: demoStart, Actor: serviceActor, Cause: "fixture"}
		_, err = repo.SetFeatureState(ctx, h, AbandonedState, "done")
		must(t, err)
		if id != descendant {
			must(t, store.SetArchived(runtime.Archive{Project: project, Workstream: id, ArchivedAt: demoStart}))
		}
	}
	cleanup := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: cfg, repository: repo, now: s.now}}
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	state, _ := store.Snapshot()
	// Deleting a workstream's own trace repository is archive-independent-deletion's
	// job (spec#6, spec#8). The retained descendant still defers the batch
	// exactly as it did before the split; what changes is that even
	// independent, which clears every deferral, stays uncleaned because its
	// trace repository is untouched until that unit deletes it.
	for _, a := range state.Archived {
		if a.Title == "" || !a.CleanedAt.IsZero() {
			t.Fatalf("cleanup with retained descendant: %+v", a)
		}
	}
	remaining, err := repo.Workstreams()
	must(t, err)
	for _, id := range all {
		if !slices.Contains(remaining, id) {
			t.Fatalf("workstream trace repository removed before archive-independent-deletion: %s", id)
		}
	}
	must(t, store.SetArchived(runtime.Archive{Project: project, Workstream: descendant, ArchivedAt: demoStart}))
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	state, _ = store.Snapshot()
	for _, a := range state.Archived {
		if a.Title == "" || !a.CleanedAt.IsZero() {
			t.Fatalf("batch marked clean before its trace repository is gone: %+v", a)
		}
	}
	remaining, err = repo.Workstreams()
	must(t, err)
	for _, id := range all {
		if !slices.Contains(remaining, id) {
			t.Fatalf("workstream trace repository removed before archive-independent-deletion: %s", id)
		}
	}
}
