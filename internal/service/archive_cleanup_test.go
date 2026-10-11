package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// captureLog redirects the standard logger to a buffer for the rest of the
// test, restoring it on cleanup. Tests that use it must not run in
// parallel with another test that also redirects the shared logger.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// awaitArchiveCleaned drives cleanup.Pass, which starts a background
// removal and lets a later pass observe its completion, until stream's
// archive is marked clean, within demoTimeout. It returns the clean
// record.
func awaitArchiveCleaned(t *testing.T, ctx context.Context, repo *trace.Repository, cleanup *archiveCleanup, store *runtime.Store, stream config.WorkstreamID) runtime.Archive {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
		state, _ := store.Snapshot()
		for _, a := range state.Archived {
			if a.Workstream == stream && !a.CleanedAt.IsZero() {
				return a
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("archive of workstream %s was never marked clean", stream)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requestRefresh durably records a pending (unresulted) librarian refresh
// covering stream's landings, the way refresher.Pass does, without
// requiring a real landing or mason report.
func requestRefresh(t *testing.T, repository *trace.Repository, librarian, stream config.WorkstreamID, at time.Time) {
	t.Helper()
	in := refreshInput{Workstream: stream, Commit: "0123456789abcdef0123456789abcdef01234567"}
	data, err := json.Marshal(in)
	must(t, err)
	id := in.key()
	eventID := trace.EventID(id, "run")
	event := trace.Event{ID: eventID, Kind: RefreshAction, Body: "Refresh local knowledge base"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(repository.Project(), librarian, eventID), Boundary: coreadapter.RunnerBoundary, Action: RefreshAction, Input: data}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repository.Project(), Workstream: librarian, At: at, Actor: serviceActor, Cause: "fixture"}
	_, err = repository.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: id, To: "requested", Reason: "refresh knowledge from workstream " + string(stream)}, Events: []trace.Event{event}})
	must(t, err)
}

func TestArchiveCleanupMigratesOldArchivesAndRetainsTitle(t *testing.T) {
	// serial: captureLog redirects the package-wide logger
	ctx := context.Background()
	logged := captureLog(t)
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
	before := strings.TrimSpace(demoGit(t, cfg.Root.String(), "-C", mustProjectTrace(t, cfg), "rev-parse", "HEAD"))
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
	if !strings.Contains(logged.String(), fmt.Sprintf("archive cleanup of workstream %s", stream)) || !strings.Contains(logged.String(), "unfinished turn") {
		t.Fatalf("deferral for a busy workstream was not logged: %s", logged.String())
	}
	// Logging the deferral never carries the planted sensitive status note
	// or document content.
	if strings.Contains(logged.String(), "Sensitive detail") || strings.Contains(logged.String(), "sensitive stdout and stderr") {
		t.Fatalf("log carries sensitive status or document content: %s", logged.String())
	}
	// The existing turn must finish before any trace or session output goes.
	must(t, repo.CaptureTurn(ctx, "title-token", trace.TurnResponse{Header: h("turn-response", "response-title"), AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "title", RequestID: "request-title", RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "test-session"}, SessionDirectory: session, StartedAt: demoStart, FinalResponse: "Done"}}))
	must(t, repo.CompleteTurn(ctx, stream, trace.ChiefOfStaff, "title", "title-token", demoStart))
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	raw, _ = store.Snapshot()
	if !raw.Archived[0].CleanedAt.IsZero() {
		t.Fatal("purged a required base")
	}
	logged.Reset()
	_, err = repo.SetWorkstreamBase(ctx, quiet, "", 1, demoStart)
	must(t, err)
	clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, stream)
	if clean.Title != "Preserved workstream title" || clean.State != AbandonedState {
		t.Fatalf("archive record after cleanup: %+v", clean)
	}
	assertGone(t, temp)
	list, err = repo.Workstreams()
	must(t, err)
	if slices.Contains(list, stream) {
		t.Fatalf("workstream trace repository not removed: %v", list)
	}
	if !slices.Contains(list, quiet) {
		t.Fatalf("an unrelated workstream's trace repository was removed: %v", list)
	}
	if _, err := os.Lstat(filepath.Join(deletingDir(t, cfg), string(stream))); !os.IsNotExist(err) {
		t.Fatalf("deleted trace left behind in the deletion staging area: %v", err)
	}
	after := strings.TrimSpace(demoGit(t, cfg.Root.String(), "-C", mustProjectTrace(t, cfg), "rev-parse", "HEAD"))
	if after != before {
		t.Fatalf("archive cleanup changed the project repository's main reference: %s, was %s", after, before)
	}
	// A repeated pass stays stable: it neither regresses what already
	// cleared nor errs now that the workstream's own trace is gone.
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	raw, _ = store.Snapshot()
	if len(raw.Archived) != 1 || raw.Archived[0].Title != "Preserved workstream title" || raw.Archived[0].CleanedAt.IsZero() {
		t.Fatalf("repeated pass changed the archive record: %+v", raw.Archived)
	}
	// The actual deletion's progress and completion are logged with the
	// workstream and project identifiers, so the absence check below is not
	// vacuously true against an empty or silently-broken log.
	if !strings.Contains(logged.String(), fmt.Sprintf("archive cleanup removed the trace of workstream %s of project %s", stream, project)) || !strings.Contains(logged.String(), fmt.Sprintf("archive cleanup completed for workstream %s of project %s", stream, project)) {
		t.Fatalf("deletion progress and completion were not logged: %s", logged.String())
	}
	// Across the start, progress and completion logging of the actual
	// deletion, the log still never carries the sensitive status note or
	// document content.
	if strings.Contains(logged.String(), "Sensitive detail") || strings.Contains(logged.String(), "sensitive stdout and stderr") {
		t.Fatalf("log carries sensitive status or document content: %s", logged.String())
	}
	got, api := s.workstreamStatus(string(stream))
	if api != nil || !got.Archived || !got.TraceDeleted || got.Status == nil || got.Status.Goal != "Preserved workstream title" || got.Status.Note != "" {
		t.Fatalf("archived list: %+v %v", got, api)
	}
	if _, api := s.unarchive(string(stream)); api == nil || api.Code != Conflict {
		t.Fatal("archive restored")
	}
}

// mustProjectTrace returns the directory of the project's own trace
// repository, a sibling of its workstreams' dedicated repositories.
func mustProjectTrace(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir, err := cfg.Root.ProjectTrace(cfg.Project.ID)
	must(t, err)
	return dir
}

// deletingDir returns the directory a workstream's trace is moved into
// while archive cleanup deletes it.
func deletingDir(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir, err := cfg.Root.WorkstreamDeleting(project)
	must(t, err)
	return dir
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
	// independent has no dependent at all and no retained base, so it is
	// deleted at once; stream and quiet stay, because quiet's retained
	// descendant still depends on it as a base, which also retains stream
	// transitively.
	independentClean, streamClean, quietClean := false, false, false
	for _, a := range state.Archived {
		if a.Title == "" {
			t.Fatalf("title not preserved: %+v", a)
		}
		switch a.Workstream {
		case independent:
			independentClean = !a.CleanedAt.IsZero()
		case stream:
			streamClean = !a.CleanedAt.IsZero()
		case quiet:
			quietClean = !a.CleanedAt.IsZero()
		}
	}
	if streamClean || quietClean {
		t.Fatalf("cleaned a base a retained descendant still depends on: %+v", state.Archived)
	}
	if !independentClean {
		// independent's own removal runs in the background; drive a few
		// more passes to let it land before asserting on it.
		awaitArchiveCleaned(t, ctx, repo, cleanup, store, independent)
	}
	remaining, err := repo.Workstreams()
	must(t, err)
	if !slices.Contains(remaining, stream) || !slices.Contains(remaining, quiet) {
		t.Fatalf("a base still depended on by a retained descendant was removed: %v", remaining)
	}
	if slices.Contains(remaining, independent) {
		t.Fatalf("an independent archived workstream with no base was not removed: %v", remaining)
	}
	must(t, store.SetArchived(runtime.Archive{Project: project, Workstream: descendant, ArchivedAt: demoStart}))
	streamClean2 := awaitArchiveCleaned(t, ctx, repo, cleanup, store, stream)
	if streamClean2.CleanedAt.IsZero() {
		t.Fatal("stream not cleaned once its retained descendant is archived too")
	}
	quietClean2 := awaitArchiveCleaned(t, ctx, repo, cleanup, store, quiet)
	if quietClean2.CleanedAt.IsZero() {
		t.Fatal("quiet not cleaned alongside its base")
	}
	descendantClean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, descendant)
	if descendantClean.CleanedAt.IsZero() {
		t.Fatal("descendant not cleaned")
	}
	remaining, err = repo.Workstreams()
	must(t, err)
	for _, id := range all {
		if slices.Contains(remaining, id) {
			t.Fatalf("workstream %s's trace repository was not removed: %v", id, remaining)
		}
	}
}

// TestArchiveCleanupDefersForAPendingOperationThenProceeds shows that
// cleanup waits, with a logged reason, for an operation without a result,
// and proceeds once it is acknowledged.
func TestArchiveCleanupDefersForAPendingOperationThenProceeds(t *testing.T) {
	// serial: captureLog redirects the package-wide logger
	ctx := context.Background()
	logged := captureLog(t)
	opts, cfg := conversationFixture(t, "archive-pending-op-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{project: {stream}}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts}
	s.setSole(runtimeFor(repo))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "done")
	must(t, err)
	requestFactoryOperation(t, repo, stream, "pending-work")
	must(t, store.SetArchived(runtime.Archive{Project: project, Workstream: stream, ArchivedAt: demoStart}))
	cleanup := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: cfg, repository: repo, now: s.now}}
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	raw, _ := store.Snapshot()
	if raw.Archived[0].Title == "" || !raw.Archived[0].CleanedAt.IsZero() {
		t.Fatalf("cleaned with an unresulted operation pending: %+v", raw.Archived)
	}
	if !strings.Contains(logged.String(), "operation without a result") {
		t.Fatalf("deferral for a pending operation was not logged: %s", logged.String())
	}
	err = repo.WithOperation(ctx, stream, "pending-work", trace.Actor{Kind: "service", ID: "test"}, s.now, func(a *trace.OperationAttempt, current trace.OperationRecord) error {
		observe := a.Action("observe", s.now())
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "not started"}
		if err := a.Record(ctx, observe); err != nil {
			return err
		}
		if err := a.Record(ctx, a.Action("effect", s.now())); err != nil {
			return err
		}
		result := a.Action("result", s.now())
		result.Result = &coreadapter.OperationResult{Outcome: "done", Evidence: "test completed it"}
		if err := a.Record(ctx, result); err != nil {
			return err
		}
		return a.Record(ctx, a.Action("acknowledge", s.now()))
	})
	must(t, err)
	logged.Reset()
	clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, stream)
	if clean.CleanedAt.IsZero() {
		t.Fatal("not cleaned once the operation was acknowledged")
	}
}

// TestArchiveCleanupDefersUntilLearningsIncorporatedThenProceeds shows that
// cleanup waits, with a logged reason, for the librarian refresh covering
// the archived workstream's landings, and proceeds once it completes.
func TestArchiveCleanupDefersUntilLearningsIncorporatedThenProceeds(t *testing.T) {
	// serial: captureLog redirects the package-wide logger
	ctx := context.Background()
	logged := captureLog(t)
	opts, cfg := conversationFixture(t, "archive-learnings-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	librarian := librarianWorkstream(project)
	must(t, repo.CreateWorkstream(ctx, librarian, demoStart, serviceActor))
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{project: {stream, librarian}}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts}
	s.setSole(runtimeFor(repo))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "done")
	must(t, err)
	requestRefresh(t, repo, librarian, stream, demoStart)
	must(t, store.SetArchived(runtime.Archive{Project: project, Workstream: stream, ArchivedAt: demoStart}))
	cleanup := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: cfg, repository: repo, now: s.now}}
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	raw, _ := store.Snapshot()
	var a runtime.Archive
	for _, candidate := range raw.Archived {
		if candidate.Workstream == stream {
			a = candidate
		}
	}
	if a.Title == "" || !a.CleanedAt.IsZero() {
		t.Fatalf("cleaned before the librarian refresh covering it completed: %+v", a)
	}
	if !strings.Contains(logged.String(), "incorporated") {
		t.Fatalf("deferral for unincorporated learnings was not logged: %s", logged.String())
	}
	in := refreshInput{Workstream: stream, Commit: "0123456789abcdef0123456789abcdef01234567"}
	err = repo.WithOperation(ctx, librarian, trace.EventID(in.key(), "run"), trace.Actor{Kind: "service", ID: "test"}, s.now, func(at *trace.OperationAttempt, current trace.OperationRecord) error {
		observe := at.Action("observe", s.now())
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "not started"}
		if err := at.Record(ctx, observe); err != nil {
			return err
		}
		if err := at.Record(ctx, at.Action("effect", s.now())); err != nil {
			return err
		}
		result := at.Action("result", s.now())
		result.Result = &coreadapter.OperationResult{Outcome: "succeeded", Evidence: "refreshed"}
		if err := at.Record(ctx, result); err != nil {
			return err
		}
		return at.Record(ctx, at.Action("acknowledge", s.now()))
	})
	must(t, err)
	logged.Reset()
	clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, stream)
	if clean.CleanedAt.IsZero() {
		t.Fatal("not cleaned once the librarian refresh completed")
	}
}

// TestArchiveCleanupDeletionResumesAfterInterruption shows that a deletion
// interrupted after its trace is moved aside, but before the moved copy is
// fully removed, is finished by a later pass -- even one belonging to a
// freshly constructed archiveCleanup, standing in for a restart -- and that
// the workstream is never again discoverable as live once the move landed.
func TestArchiveCleanupDeletionResumesAfterInterruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "archive-interrupt-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{project: {stream}}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts}
	s.setSole(runtimeFor(repo))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "done")
	must(t, err)
	must(t, store.SetArchived(runtime.Archive{Project: project, Workstream: stream, ArchivedAt: demoStart}))
	// A deletion whose removal is interrupted after the trace is moved
	// aside, but before any of it is actually removed, by a finish hook
	// that fails the first time -- standing in for a crash mid-removal.
	interrupted := false
	cleanup := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: cfg, repository: repo, now: s.now}, finish: func(root config.Root, p config.ProjectID, w config.WorkstreamID) error {
		if !interrupted {
			interrupted = true
			return fmt.Errorf("simulated crash mid-removal")
		}
		return trace.FinishWorkstreamTraceDeletion(root, p, w)
	}}
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	// Give the backgrounded, failing removal a moment to run and record
	// its failure, then confirm the move already landed: the workstream is
	// absent from the live listing even though its trace is not yet gone.
	deadline := time.Now().Add(demoTimeout)
	for {
		if present, err := os.Lstat(filepath.Join(deletingDir(t, cfg), string(stream))); err == nil {
			_ = present
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("trace never moved aside for deletion")
		}
		time.Sleep(20 * time.Millisecond)
	}
	list, err := repo.Workstreams()
	must(t, err)
	if slices.Contains(list, stream) {
		t.Fatal("a workstream whose trace moved aside for deletion is still listed as live")
	}
	if _, err := repo.Operations(stream); err == nil {
		t.Fatal("a workstream whose trace moved aside for deletion is still readable as live")
	}
	// A fresh archiveCleanup, standing in for a service restarted after the
	// crash, finishes the removal that was left incomplete.
	restarted := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: cfg, repository: repo, now: s.now}}
	clean := awaitArchiveCleaned(t, ctx, repo, restarted, store, stream)
	if clean.CleanedAt.IsZero() {
		t.Fatal("interrupted deletion never completed after restart")
	}
	if _, err := os.Lstat(filepath.Join(deletingDir(t, cfg), string(stream))); !os.IsNotExist(err) {
		t.Fatalf("deleted trace left behind in the deletion staging area: %v", err)
	}
}

// TestArchiveCleanupLargeDeletionDoesNotDelayReadsOrOtherWrites shows that
// deleting a large archived workstream's trace, held in flight by a
// blocked finish hook, does not delay an ordinary status read or a write
// to an unrelated workstream.
func TestArchiveCleanupLargeDeletionDoesNotDelayReadsOrOtherWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "archive-responsive-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{project: {stream, quiet}}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts}
	s.setSole(runtimeFor(repo))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "finish", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "done")
	must(t, err)
	must(t, store.SetArchived(runtime.Archive{Project: project, Workstream: stream, ArchivedAt: demoStart}))
	hold := make(chan struct{})
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	cleanup := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: cfg, repository: repo, now: s.now}, finish: func(root config.Root, p config.ProjectID, w config.WorkstreamID) error {
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
	must(t, repo.Serialize(func() error { return cleanup.Pass(ctx) }))
	select {
	case <-started:
	case <-time.After(demoTimeout):
		t.Fatal("the background removal never started")
	}
	// The removal is now held in flight, under whatever lock Pass ran
	// serialized with. A status read and a write to the unrelated
	// workstream must still complete promptly.
	begin := time.Now()
	if _, _, api := s.statuses(); api != nil {
		t.Fatalf("status read failed while a deletion was in flight: %v", api)
	}
	must(t, repo.Append(ctx, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "note", Revision: 1, Project: project, Workstream: quiet, At: demoStart, Actor: serviceActor, Cause: "fixture"}, Path: "handed/note.json", Content: "unrelated workstream progress"}))
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("a status read and an unrelated workstream's write waited %s on a deletion in flight", elapsed)
	}
	close(hold)
	clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, stream)
	if clean.CleanedAt.IsZero() {
		t.Fatal("deletion never completed once released")
	}
}

// TestArchiveRecordsOutcomeAndPullRequestLinkThenCleansUp shows that
// archiving a delivered workstream with its own pull request keeps a
// record with its project, ID, title, outcome, archive time and the pull
// request's URL, and that cleanup then deletes its trace repository while
// the archived list still shows it, with that link intact.
func TestArchiveRecordsOutcomeAndPullRequestLinkThenCleansUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "archive-record-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: map[config.ProjectID][]config.WorkstreamID{project: {stream}}})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts}
	s.setSole(runtimeFor(repo))
	const pullRequestURL = "https://github.com/owner/osmia/pull/7"
	record := DeliveryPublication{Approval: 1, Operation: "publish-1", Status: publicationOpened, Style: "commit-per-unit", Fork: "owner/fork", Remote: "origin", Branch: featureBranch(stream), Commit: "0123456789abcdef0123456789abcdef01234567", PullRequest: 7, URL: pullRequestURL, Title: "Ship it", Description: "a change", DescriptionHash: "hash"}
	data, err := json.Marshal(record)
	must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: publicationDocument, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: foremanActor, Cause: "publish-1"}, Path: publicationPath, Content: string(data)}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: foremanActor, Cause: "publish-1"}
	_, err = repo.RecordDocumentsWith(ctx, []trace.Document{doc}, trace.Transaction{Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, From: "", To: DeliveredState, Reason: "delivered for the test"}})
	must(t, err)
	resp, api := s.archive(string(stream))
	if api != nil || !resp.Archived {
		t.Fatalf("archive: %+v %v", resp, api)
	}
	raw, _ := store.Snapshot()
	if len(raw.Archived) != 1 {
		t.Fatalf("archive record: %+v", raw.Archived)
	}
	a := raw.Archived[0]
	if a.Project != project || a.Workstream != stream || a.Title == "" || a.State != DeliveredState || a.ArchivedAt.IsZero() || a.DeliveryLink != pullRequestURL {
		t.Fatalf("archive record: %+v", a)
	}
	cleanup := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: cfg, repository: repo, now: s.now}}
	clean := awaitArchiveCleaned(t, ctx, repo, cleanup, store, stream)
	if clean.DeliveryLink != pullRequestURL {
		t.Fatalf("delivery link lost after cleanup: %+v", clean)
	}
	got, api := s.workstreamStatus(string(stream))
	if api != nil || !got.Archived || !got.TraceDeleted || got.DeliveryLink != pullRequestURL {
		t.Fatalf("archived list after cleanup: %+v %v", got, api)
	}
}
