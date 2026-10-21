package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// blockArchiveCleanup durably records, on stream, an operation whose
// boundary (coreadapter.ContainerBoundary) no production or test adapter is
// ever configured for, so left alone the reconcile controller finds it due,
// observes coreadapter.EffectUnknown and retries it forever without
// acknowledging it or failing the loop (internal/reconcile/controller.go,
// reconcile). Until it is acknowledged, archiveCleanup.prepare always
// defers stream's archive with "the workstream has an unacknowledged
// operation" (archive_cleanup.go, prepare), regardless of how many
// background passes a live service runs concurrently with this test. This
// replaces relying on timing to keep a just-archived workstream's trace
// from being deleted by the live reconcile loop before the test gets to
// assert on it.
func blockArchiveCleanup(t *testing.T, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, at time.Time, actor trace.Actor) {
	t.Helper()
	const eventID = "block-archive-cleanup"
	event := trace.Event{ID: eventID, Kind: "local-effect", Body: "hold archive cleanup for the test"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(project, stream, event.ID), Boundary: coreadapter.ContainerBoundary, Action: "fixture-hold", Input: json.RawMessage(`{}`)}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: eventID + "-start", Revision: 1, Project: project, Workstream: stream, At: at, Actor: actor, Cause: "fixture"}
	_, err := repository.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: "fixture-" + eventID, To: "pending", Reason: "test fixture hold"}, Events: []trace.Event{event}})
	must(t, err)
}

// unblockArchiveCleanup acknowledges the operation blockArchiveCleanup
// recorded, the way a real reconciler would once its effect completed, so
// archiveCleanup.prepare can report stream eligible for cleanup again.
func unblockArchiveCleanup(t *testing.T, ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, now func() time.Time) {
	t.Helper()
	err := repository.WithOperation(ctx, stream, "block-archive-cleanup", trace.Actor{Kind: "service", ID: "test"}, now, func(a *trace.OperationAttempt, current trace.OperationRecord) error {
		observe := a.Action("observe", now())
		observe.Observation = &coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "not started"}
		if err := a.Record(ctx, observe); err != nil {
			return err
		}
		if err := a.Record(ctx, a.Action("effect", now())); err != nil {
			return err
		}
		result := a.Action("result", now())
		result.Result = &coreadapter.OperationResult{Outcome: "done", Evidence: "test fixture released it"}
		if err := a.Record(ctx, result); err != nil {
			return err
		}
		return a.Record(ctx, a.Action("acknowledge", now()))
	})
	must(t, err)
}

// archiveRecord returns stream's archive record from store's current
// snapshot, and whether it has one at all.
func archiveRecord(t *testing.T, store *runtime.Store, stream config.WorkstreamID) (runtime.Archive, bool) {
	t.Helper()
	return intakefixture.ArchiveRecord(t, store, stream)
}

// workstreamTraceUnchanged fails t if the workstream's own trace holds a
// commit it did not hold before: migration must never write to it.
func workstreamTraceUnchanged(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID, before int) {
	t.Helper()
	if after := traceCommits(t, workstreamTraceDirectory(t, root, project, stream)); after != before {
		t.Fatalf("workstream %s trace went from %d to %d commits during migration", stream, before, after)
	}
}

// TestStartupWrapsExistingWorkstreamsInOneWorkgroupEachAndExcludesSessions
// covers spec#3: on start, every existing non-session workstream of a
// one-repo project is wrapped in its own one-workstream workgroup with no
// owner action, a session workstream is listed outside any workgroup, and
// nothing about any workstream's own trace changes.
func TestStartupWrapsExistingWorkstreamsInOneWorkgroupEachAndExcludesSessions(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	ctx := context.Background()
	// Holding every coreadapter.ContainerBoundary operation keeps the live
	// reconcile loop from ever claiming the one blockArchiveCleanup records
	// below, so this test's own later unblockArchiveCleanup call cannot
	// race a concurrent claim attempt by that loop.
	opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		return op.Boundary == coreadapter.ContainerBoundary
	}

	s, c := start(t, opts)
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	project := added.Project.ID

	active := s.sole()
	if active == nil {
		t.Fatal("no active project")
	}
	building := config.WorkstreamID("w_00000000000000000000000000000011")
	second := config.WorkstreamID("w_00000000000000000000000000000012")
	session := config.WorkstreamID("w_00000000000000000000000000000013")
	delivered := config.WorkstreamID("w_00000000000000000000000000000014")
	abandoned := config.WorkstreamID("w_00000000000000000000000000000015")
	archived := config.WorkstreamID("w_00000000000000000000000000000016")
	must(t, active.repository.CreateWorkstream(ctx, building, s.now(), registrationActor))
	must(t, active.repository.CreateWorkstream(ctx, second, s.now(), registrationActor))
	must(t, active.repository.CreateSessionWorkstream(ctx, session, "default", "deadbeef", "osmia/session/"+string(session), "a session", s.now(), registrationActor))
	must(t, active.repository.CreateWorkstream(ctx, delivered, s.now(), registrationActor))
	must(t, active.repository.CreateWorkstream(ctx, abandoned, s.now(), registrationActor))
	must(t, active.repository.CreateWorkstream(ctx, archived, s.now(), registrationActor))
	deliveredHeader := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-fixture", Revision: 1, Project: project, Workstream: delivered, At: s.now(), Actor: registrationActor, Cause: "fixture"}
	_, err = active.repository.SetFeatureState(ctx, deliveredHeader, DeliveredState, "pull request merged")
	must(t, err)
	abandonedHeader := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "abandon-fixture", Revision: 1, Project: project, Workstream: abandoned, At: s.now(), Actor: registrationActor, Cause: "fixture"}
	_, err = active.repository.SetFeatureState(ctx, abandonedHeader, AbandonedState, "superseded")
	must(t, err)
	archivedHeader := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "archive-fixture", Revision: 1, Project: project, Workstream: archived, At: s.now(), Actor: registrationActor, Cause: "fixture"}
	_, err = active.repository.SetFeatureState(ctx, archivedHeader, AbandonedState, "superseded")
	must(t, err)
	// Held open until the very end of this test (see unblockArchiveCleanup
	// below), so archiveCleanup.prepare always defers archived's cleanup
	// deterministically, regardless of how many passes the live reconcile
	// loop runs concurrently with the rest of this test.
	blockArchiveCleanup(t, active.repository, project, archived, s.now(), registrationActor)
	// refreshRuntime re-reads the trace's workstreams so the runtime store,
	// whose inputs were frozen at Start, recognizes archived as a valid
	// archive target.
	must(t, s.refreshRuntime())
	// Title and State are set up front, as a real archive always has them
	// (projects.go, archive) by the time it exists: an empty Title is only
	// the pre-migration shape archiveCleanup.prepare fills in by writing
	// through c.s.store.UpdateArchive on its first look, which runs whether
	// or not the record is otherwise eligible and would otherwise race this
	// test's own "before" and "after" reads of the very same field.
	must(t, s.store.SetArchived(runtime.Archive{Project: project, Workstream: archived, ArchivedAt: s.now(), Title: "Archived fixture workstream", State: AbandonedState}))
	archiveBefore, ok := archiveRecord(t, s.store, archived)
	if !ok {
		t.Fatal("archive record missing before migration")
	}

	documentsBefore := map[string][]byte{}
	for _, name := range []string{"charter.md", trace.EntitiesPath} {
		content, err := projectDocument(t, s.cfg.Root, project, name)
		must(t, err)
		documentsBefore[name] = content
	}
	buildingBefore := traceCommits(t, workstreamTraceDirectory(t, s.cfg.Root, project, building))
	secondBefore := traceCommits(t, workstreamTraceDirectory(t, s.cfg.Root, project, second))
	sessionBefore := traceCommits(t, workstreamTraceDirectory(t, s.cfg.Root, project, session))
	deliveredBefore := traceCommits(t, workstreamTraceDirectory(t, s.cfg.Root, project, delivered))
	abandonedBefore := traceCommits(t, workstreamTraceDirectory(t, s.cfg.Root, project, abandoned))
	archivedBefore := traceCommits(t, workstreamTraceDirectory(t, s.cfg.Root, project, archived))

	must(t, s.Close())
	s, c = start(t, opts)
	_ = c

	active = s.sole()
	if active == nil {
		t.Fatal("no active project after restart")
	}
	groups, revision, err := trace.Workgroups(active.repository)
	must(t, err)
	if revision != 1 || len(groups) != 5 {
		t.Fatalf("workgroups after migration: revision=%d groups=%+v", revision, groups)
	}
	for _, stream := range []config.WorkstreamID{building, second, delivered, abandoned, archived} {
		g, ok := trace.WorkgroupOf(groups, stream)
		if !ok || len(g.Members) != 1 || g.Members[0] != stream {
			t.Fatalf("workstream %s workgroup: %+v ok=%v", stream, g, ok)
		}
		if err := config.CheckWorkgroupIDs(g.ID); err != nil {
			t.Fatalf("workgroup identity: %v", err)
		}
	}
	if _, ok := trace.WorkgroupOf(groups, session); ok {
		t.Fatal("session workstream was wrapped in a workgroup")
	}

	// The archived workstream's archive record is byte-identical after
	// migration, and it is still present in the trace -- the precondition
	// archive cleanup requires to consider it eligible for cleanup.
	archiveAfter, ok := archiveRecord(t, s.store, archived)
	if !ok {
		t.Fatal("archive record lost by migration")
	}
	if archiveAfter.Project != archiveBefore.Project || archiveAfter.Workstream != archiveBefore.Workstream ||
		archiveAfter.Title != archiveBefore.Title || archiveAfter.State != archiveBefore.State ||
		archiveAfter.DeliveryLink != archiveBefore.DeliveryLink ||
		!archiveAfter.ArchivedAt.Equal(archiveBefore.ArchivedAt) || !archiveAfter.CleanedAt.Equal(archiveBefore.CleanedAt) {
		t.Fatalf("archive record changed by migration: before=%+v after=%+v", archiveBefore, archiveAfter)
	}
	list, err := active.repository.Workstreams()
	must(t, err)
	if !slices.Contains(list, archived) {
		t.Fatal("archived workstream's trace was removed by migration")
	}

	// The charter and the entity map are unchanged: migration only ever
	// adds workgroups.json as a new revision.
	for name, before := range documentsBefore {
		after, err := projectDocument(t, s.cfg.Root, project, name)
		must(t, err)
		if string(after) != string(before) {
			t.Fatalf("%s changed by migration", name)
		}
	}
	// Each workstream's own dedicated trace is untouched.
	workstreamTraceUnchanged(t, s.cfg.Root, project, building, buildingBefore)
	workstreamTraceUnchanged(t, s.cfg.Root, project, second, secondBefore)
	workstreamTraceUnchanged(t, s.cfg.Root, project, session, sessionBefore)
	workstreamTraceUnchanged(t, s.cfg.Root, project, delivered, deliveredBefore)
	workstreamTraceUnchanged(t, s.cfg.Root, project, abandoned, abandonedBefore)
	workstreamTraceUnchanged(t, s.cfg.Root, project, archived, archivedBefore)

	// Restarting again changes nothing further and duplicates nothing.
	must(t, s.Close())
	s, c = start(t, opts)
	defer func() { must(t, s.Close()) }()
	_ = c
	active = s.sole()
	again, revisionAgain, err := trace.Workgroups(active.repository)
	must(t, err)
	if revisionAgain != revision || len(again) != len(groups) {
		t.Fatalf("restart changed workgroups: before=%+v after=%+v", groups, again)
	}
	for _, g := range groups {
		found, ok := trace.WorkgroupOf(again, g.Members[0])
		if !ok || found.ID != g.ID {
			t.Fatalf("restart changed workgroup identity for %s: %+v vs %+v", g.Members[0], g, found)
		}
	}

	// Only now, with every other assertion already made against archived's
	// still-present trace, release the hold: archiveCleanup.prepare, called
	// directly so this does not depend on the live loop's own timing, must
	// report archived genuinely eligible for cleanup.
	unblockArchiveCleanup(t, ctx, active.repository, archived, s.now)
	archiveReleased, ok := archiveRecord(t, s.store, archived)
	if !ok {
		t.Fatal("archive record missing after releasing the hold")
	}
	cleanup := &archiveCleanup{s: s, workspaces: &workspaceCleanup{cfg: s.cfg, repository: active.repository, now: s.now}}
	eligible, reason, err := cleanup.prepare(ctx, archiveReleased)
	must(t, err)
	if !eligible {
		t.Fatalf("archived workstream not eligible for archive cleanup after migration: %s", reason)
	}
}

// TestStartupMigrationResumesAfterAnInjectedCrashWithoutDuplicating covers the
// resumable half of spec#3: a crash that stops migration after one project
// completes, but before a second project's is recorded, is resumed cleanly
// on the next start, leaving the first project's workgroup untouched and
// completing the second.
func TestStartupMigrationResumesAfterAnInjectedCrashWithoutDuplicating(t *testing.T) {
	// serial: sets the package-level hook lifecycle.EnsureWorkgroupsInterrupt
	opts, clone := projectFixture(t)
	ctx := context.Background()

	s, c := start(t, opts)
	first, err := c.AddProject(ctx, request(clone))
	must(t, err)
	secondClone := filepath.Join(filepath.Dir(clone), "other")
	demoGit(t, filepath.Dir(clone), "init", "--quiet", secondClone)
	secondAdded, err := c.AddProject(ctx, ProjectAddRequest{Name: "other", Upstream: "upstream/other", Fork: "owner/other", Clone: secondClone})
	must(t, err)

	var firstActive, secondActive *activeProject
	for _, p := range s.projectViews() {
		switch p.id {
		case first.Project.ID:
			firstActive = p
		case secondAdded.Project.ID:
			secondActive = p
		}
	}
	if firstActive == nil || secondActive == nil {
		t.Fatal("both projects must be active")
	}
	firstStream := config.WorkstreamID("w_00000000000000000000000000000021")
	secondStream := config.WorkstreamID("w_00000000000000000000000000000022")
	must(t, firstActive.repository.CreateWorkstream(ctx, firstStream, s.now(), registrationActor))
	must(t, secondActive.repository.CreateWorkstream(ctx, secondStream, s.now(), registrationActor))
	must(t, s.Close())

	// The second start injects a failure right before the second project's
	// repository (in active_projects order, after the first) is recorded,
	// simulating a crash partway through the migration.
	lifecycle.EnsureWorkgroupsInterrupt = func(id config.ProjectID) error {
		if id == secondAdded.Project.ID {
			return errors.New("simulated crash mid-migration")
		}
		return nil
	}
	_, err = Start(ctx, opts)
	if err == nil {
		t.Fatal("expected the injected crash to fail startup")
	}
	lifecycle.EnsureWorkgroupsInterrupt = nil

	s, c = start(t, opts)
	defer func() { must(t, s.Close()) }()
	_ = c

	var firstAfter, secondAfter *activeProject
	for _, p := range s.projectViews() {
		switch p.id {
		case first.Project.ID:
			firstAfter = p
		case secondAdded.Project.ID:
			secondAfter = p
		}
	}
	if firstAfter == nil || secondAfter == nil {
		t.Fatal("both projects must still be active")
	}
	firstGroups, firstRevision, err := trace.Workgroups(firstAfter.repository)
	must(t, err)
	if firstRevision != 1 || len(firstGroups) != 1 {
		t.Fatalf("first project's workgroups after resumed migration: revision=%d groups=%+v", firstRevision, firstGroups)
	}
	if g, ok := trace.WorkgroupOf(firstGroups, firstStream); !ok || len(g.Members) != 1 {
		t.Fatalf("first project's workstream workgroup: %+v ok=%v", g, ok)
	}
	secondGroups, secondRevision, err := trace.Workgroups(secondAfter.repository)
	must(t, err)
	if secondRevision != 1 || len(secondGroups) != 1 {
		t.Fatalf("second project's workgroups after resumed migration: revision=%d groups=%+v", secondRevision, secondGroups)
	}
	if g, ok := trace.WorkgroupOf(secondGroups, secondStream); !ok || len(g.Members) != 1 {
		t.Fatalf("second project's workstream workgroup: %+v ok=%v", g, ok)
	}
}

// TestWorkgroupResolutionFiresOnlyAfterEveryMemberIsResolvedThroughALivePass
// covers acceptance item 5 and spec#22's resolution trigger through the
// running service's own reconciliation pass, not a direct function call: a
// two-workstream workgroup's resolution is not recorded while only one
// member is delivered, is recorded exactly once once both are, and both
// members' traces stay fully readable throughout, since nothing here
// deletes, archives or otherwise retires anything.
func TestWorkgroupResolutionFiresOnlyAfterEveryMemberIsResolvedThroughALivePass(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	var passed atomic.Int64
	opts.schedulePassed = func() { passed.Add(1) }
	ctx := context.Background()

	s, c := start(t, opts)
	defer func() { must(t, s.Close()) }()
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	project := added.Project.ID

	active := s.sole()
	if active == nil {
		t.Fatal("no active project")
	}
	first := config.WorkstreamID("w_00000000000000000000000000000031")
	second := config.WorkstreamID("w_00000000000000000000000000000032")
	must(t, active.repository.CreateWorkstream(ctx, first, s.now(), registrationActor))
	must(t, active.repository.CreateWorkstream(ctx, second, s.now(), registrationActor))
	g, err := trace.CreateWorkgroup(ctx, active.repository, []config.WorkstreamID{first, second}, s.now(), registrationActor, "test-workgroup")
	must(t, err)

	deliverFirst := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-first", Revision: 1, Project: project, Workstream: first, At: s.now(), Actor: registrationActor, Cause: "fixture"}
	_, err = active.repository.SetFeatureState(ctx, deliverFirst, DeliveredState, "pull request merged")
	must(t, err)
	recordResolutionMerge(t, active.repository, first)

	awaitPasses(t, &passed, 2)
	resolved, _, err := trace.ResolvedWorkgroups(active.repository)
	must(t, err)
	if len(resolved) != 0 {
		t.Fatalf("workgroup resolved after only one of its two members delivered: %+v", resolved)
	}
	if _, err := active.repository.Workstreams(); err != nil {
		t.Fatalf("workstreams unreadable while one member is unresolved: %v", err)
	}

	deliverSecond := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-second", Revision: 1, Project: project, Workstream: second, At: s.now(), Actor: registrationActor, Cause: "fixture"}
	_, err = active.repository.SetFeatureState(ctx, deliverSecond, DeliveredState, "pull request merged")
	must(t, err)
	recordResolutionMerge(t, active.repository, second)

	awaitPasses(t, &passed, 2)
	resolved, _, err = trace.ResolvedWorkgroups(active.repository)
	must(t, err)
	if len(resolved) != 1 || resolved[0] != g.ID {
		t.Fatalf("workgroup not resolved, exactly once, after both members delivered: %+v, want [%s]", resolved, g.ID)
	}

	streams, err := active.repository.Workstreams()
	must(t, err)
	if !slices.Contains(streams, first) || !slices.Contains(streams, second) {
		t.Fatalf("both members' traces must remain readable after the workgroup resolves: %+v", streams)
	}
	for _, stream := range []config.WorkstreamID{first, second} {
		if _, err := trace.Read[trace.Document](active.repository, stream); err != nil {
			t.Fatalf("workstream %s's trace unreadable after the workgroup resolves: %v", stream, err)
		}
	}

	// A later pass changes nothing further and does not mark the workgroup
	// resolved twice.
	awaitPasses(t, &passed, 2)
	resolved, _, err = trace.ResolvedWorkgroups(active.repository)
	must(t, err)
	if len(resolved) != 1 {
		t.Fatalf("later passes resolved the workgroup more than once: %+v", resolved)
	}
}

// TestWorkgroupResolutionCountsAMemberArchiveCleanupAlreadyDeletedAsResolved
// counts a missing member only after owner archive and eligible production
// cleanup record its resolution and deletion. Unexplained absence is an error.
func TestWorkgroupResolutionCountsAMemberArchiveCleanupAlreadyDeletedAsResolved(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	ctx := context.Background()
	setupReady := make(chan struct{})
	setupRelease := make(chan struct{})
	gate := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(gate.release)
	opts.boundary = gate.checkpoint
	var passed atomic.Int64
	passChanges := make(chan struct{}, 1)
	opts.schedulePassed = func() {
		passed.Add(1)
		select {
		case passChanges <- struct{}{}:
		default:
		}
	}
	var setupOnce sync.Once
	opts.Reconciliation.Schedule = func(ctx context.Context) error {
		setupOnce.Do(func() { close(setupReady) })
		select {
		case <-setupRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s, c := start(t, opts)
	t.Cleanup(func() { must(t, s.Close()) })
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	project := added.Project.ID
	select {
	case <-setupReady:
	case <-time.After(demoTimeout):
		t.Fatal("live controller did not reach the setup boundary")
	}
	active := s.sole()
	if active == nil {
		t.Fatal("no active project")
	}
	kept := config.WorkstreamID("w_00000000000000000000000000000041")
	gone := config.WorkstreamID("w_00000000000000000000000000000042")
	must(t, active.repository.CreateWorkstream(ctx, kept, s.now(), registrationActor))
	must(t, active.repository.CreateWorkstream(ctx, gone, s.now(), registrationActor))
	g, err := trace.CreateWorkgroup(ctx, active.repository, []config.WorkstreamID{kept, gone}, s.now(), registrationActor, "test-workgroup")
	must(t, err)
	deliverGone := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-gone", Revision: 1, Project: project, Workstream: gone, At: s.now(), Actor: registrationActor, Cause: "fixture"}
	_, err = active.repository.SetFeatureState(ctx, deliverGone, DeliveredState, "pull request merged")
	must(t, err)
	recordResolutionMerge(t, active.repository, gone)
	must(t, s.refreshRuntime())
	_, err = c.Archive(ctx, gone)
	must(t, err)
	close(setupRelease)
	gate.await(t)
	gate.release()
	changes, closeChanges := serviceChanges(t, s)
	defer closeChanges()
	await := func(phase string, ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(demoTimeout)
		for !ready() {
			if time.Now().After(deadline) {
				t.Fatalf("live controller did not complete %s", phase)
			}
			changes()
		}
	}
	await("owner-authorized archive cleanup", func() bool {
		a, found := archiveRecord(t, s.store, gone)
		return found && a.Resolved && !a.CleanupStartedAt.IsZero() && !a.CleanedAt.IsZero()
	})
	streams, err := active.repository.Workstreams()
	must(t, err)
	if slices.Contains(streams, gone) {
		t.Fatalf("gone's trace must be fully deleted for this test: %+v", streams)
	}
	resolved, _, err := trace.ResolvedWorkgroups(active.repository)
	must(t, err)
	if len(resolved) != 0 {
		t.Fatalf("workgroup resolved while its other member is still unresolved: %+v", resolved)
	}
	deliverKept := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-kept", Revision: 1, Project: project, Workstream: kept, At: s.now(), Actor: registrationActor, Cause: "fixture"}
	_, err = active.repository.SetFeatureState(ctx, deliverKept, DeliveredState, "pull request merged")
	must(t, err)
	recordResolutionMerge(t, active.repository, kept)
	await("final group resolution", func() bool {
		resolved, _, err = trace.ResolvedWorkgroups(active.repository)
		must(t, err)
		return len(resolved) != 0
	})
	if len(resolved) != 1 || resolved[0] != g.ID {
		t.Fatalf("workgroup not resolved, exactly once, once its remaining member is delivered: %+v, want [%s]", resolved, g.ID)
	}
	// Observe two further completed passes from the same live controller.
	target := passed.Load() + 2
	deadline := time.NewTimer(demoTimeout)
	defer deadline.Stop()
	for passed.Load() < target {
		select {
		case <-passChanges:
		case <-deadline.C:
			t.Fatal("live controller did not finish later passes")
		}
	}
	resolved, _, err = trace.ResolvedWorkgroups(active.repository)
	must(t, err)
	if len(resolved) != 1 {
		t.Fatalf("later passes resolved the workgroup more than once: %+v", resolved)
	}
}

// TestCrossRepoWorkgroupResolutionFiresOnlyAfterEveryMemberIsResolved covers
// workgroupResolver.Pass's cross-repo half: a workgroup whose two members
// live in different projects' own repositories is not resolved while only
// one of them has delivered, and is resolved, exactly once and recorded in
// the workgroup's own trace rather than either member's, once the second
// also delivers. Either member's own project running the pass is enough:
// each finds the same workgroup through its own cross-repo pointer and
// tries the same idempotent record.
func TestCrossRepoWorkgroupResolutionFiresOnlyAfterEveryMemberIsResolved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone1 := projectFixture(t)
	home := filepath.Dir(clone1)
	clone2 := filepath.Join(home, "other")
	demoGit(t, home, "init", "--quiet", clone2)
	var passed atomic.Int64
	opts.schedulePassed = func() { passed.Add(1) }

	s, c := start(t, opts)
	added1, err := c.AddProject(ctx, request(clone1))
	must(t, err)
	added2, err := c.AddProject(ctx, ProjectAddRequest{Name: "other", Upstream: "upstream/other", Fork: "owner/other", Clone: clone2})
	must(t, err)
	_, active1 := s.runtimeOf(added1.Project.ID)
	_, active2 := s.runtimeOf(added2.Project.ID)
	if active1 == nil || active2 == nil {
		t.Fatal("both projects must be active")
	}

	first := config.WorkstreamID("w_00000000000000000000000000000061")
	second := config.WorkstreamID("w_00000000000000000000000000000062")
	must(t, active1.repository.CreateWorkstream(ctx, first, s.now(), registrationActor))
	must(t, active2.repository.CreateWorkstream(ctx, second, s.now(), registrationActor))

	id, api := s.DraftWorkgroup(ctx, []config.WorkstreamID{first, second})
	if api != nil {
		t.Fatalf("draft workgroup: %v", api)
	}

	resolvedIDs := func() []config.WorkgroupID {
		t.Helper()
		members := []trace.CrossRepoMember{{Workstream: first, Project: active1.repository}, {Workstream: second, Project: active2.repository}}
		wt, err := trace.OpenWorkgroupTrace(s.current().Root, id, members)
		must(t, err)
		defer wt.Close()
		ids, _, err := trace.ResolvedWorkgroups(wt)
		must(t, err)
		return ids
	}

	awaitPasses(t, &passed, 2)
	if ids := resolvedIDs(); len(ids) != 0 {
		t.Fatalf("workgroup resolved before either member delivered: %+v", ids)
	}

	deliverFirst := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-first", Revision: 1, Project: added1.Project.ID, Workstream: first, At: s.now(), Actor: registrationActor, Cause: "fixture"}
	_, err = active1.repository.SetFeatureState(ctx, deliverFirst, DeliveredState, "pull request merged")
	must(t, err)
	recordResolutionMerge(t, active1.repository, first)

	awaitPasses(t, &passed, 4)
	if ids := resolvedIDs(); len(ids) != 0 {
		t.Fatalf("workgroup resolved after only one of its two members delivered: %+v", ids)
	}
	if _, err := active1.repository.Workstreams(); err != nil {
		t.Fatalf("workstreams unreadable while one member is unresolved: %v", err)
	}

	deliverSecond := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-second", Revision: 1, Project: added2.Project.ID, Workstream: second, At: s.now(), Actor: registrationActor, Cause: "fixture"}
	_, err = active2.repository.SetFeatureState(ctx, deliverSecond, DeliveredState, "pull request merged")
	must(t, err)
	recordResolutionMerge(t, active2.repository, second)

	awaitPasses(t, &passed, 4)
	ids := resolvedIDs()
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("workgroup not resolved, exactly once, after both members delivered: %+v, want [%s]", ids, id)
	}

	// A later pass changes nothing further and does not mark the workgroup
	// resolved twice.
	awaitPasses(t, &passed, 4)
	if ids := resolvedIDs(); len(ids) != 1 {
		t.Fatalf("later passes resolved the workgroup more than once: %+v", ids)
	}
}
