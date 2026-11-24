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

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func blockArchiveCleanup(t *testing.T, repository *trace.Repository, project config.ProjectID, stream config.WorkstreamID, at time.Time, actor trace.Actor) {
	t.Helper()
	const eventID = "block-archive-cleanup"
	event := trace.Event{ID: eventID, Kind: "local-effect", Body: "hold archive cleanup for the test"}
	event.Operation = &coreadapter.Operation{ID: trace.OperationID(project, stream, event.ID), Boundary: coreadapter.ContainerBoundary, Action: "fixture-hold", Input: json.RawMessage(`{}`)}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: eventID + "-start", Revision: 1, Project: project, Workstream: stream, At: at, Actor: actor, Cause: "fixture"}
	_, err := repository.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: "fixture-" + eventID, To: "pending", Reason: "test fixture hold"}, Events: []trace.Event{event}})
	fixturekit.Must(t, err)
}

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
	fixturekit.Must(t, err)
}

func workstreamTraceUnchanged(t *testing.T, root config.Root, project config.ProjectID, stream config.WorkstreamID, before int) {
	t.Helper()
	if after := fixturekit.TraceCommits(t, fixturekit.WorkstreamTraceDirectory(t, root, project, stream)); after != before {
		t.Fatalf("workstream %s trace went from %d to %d commits during migration", stream, before, after)
	}
}

func TestStartupWrapsExistingWorkstreamsInOneWorkgroupEachAndExcludesSessions(t *testing.T) {
	t.Parallel()
	opts, clone := journey.ProjectFixture(t)
	ctx := context.Background()
	opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		return op.Boundary == coreadapter.ContainerBoundary
	}

	s, c := journey.StartT(t, opts)
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	project := added.Project.ID

	active := s.Sole()
	if active == nil {
		t.Fatal("no active project")
	}
	building := config.WorkstreamID("w_00000000000000000000000000000011")
	second := config.WorkstreamID("w_00000000000000000000000000000012")
	session := config.WorkstreamID("w_00000000000000000000000000000013")
	delivered := config.WorkstreamID("w_00000000000000000000000000000014")
	abandoned := config.WorkstreamID("w_00000000000000000000000000000015")
	archived := config.WorkstreamID("w_00000000000000000000000000000016")
	fixturekit.Must(t, active.Repository.CreateWorkstream(ctx, building, s.Now(), intake.RegistrationActor))
	fixturekit.Must(t, active.Repository.CreateWorkstream(ctx, second, s.Now(), intake.RegistrationActor))
	fixturekit.Must(t, active.Repository.CreateSessionWorkstream(ctx, session, "default", "deadbeef", "osmia/session/"+string(session), "a session", s.Now(), intake.RegistrationActor))
	fixturekit.Must(t, active.Repository.CreateWorkstream(ctx, delivered, s.Now(), intake.RegistrationActor))
	fixturekit.Must(t, active.Repository.CreateWorkstream(ctx, abandoned, s.Now(), intake.RegistrationActor))
	fixturekit.Must(t, active.Repository.CreateWorkstream(ctx, archived, s.Now(), intake.RegistrationActor))
	deliveredHeader := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-fixture", Revision: 1, Project: project, Workstream: delivered, At: s.Now(), Actor: intake.RegistrationActor, Cause: "fixture"}
	_, err = active.Repository.SetFeatureState(ctx, deliveredHeader, intake.DeliveredState, "pull request merged")
	fixturekit.Must(t, err)
	abandonedHeader := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "abandon-fixture", Revision: 1, Project: project, Workstream: abandoned, At: s.Now(), Actor: intake.RegistrationActor, Cause: "fixture"}
	_, err = active.Repository.SetFeatureState(ctx, abandonedHeader, intake.AbandonedState, "superseded")
	fixturekit.Must(t, err)
	archivedHeader := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "archive-fixture", Revision: 1, Project: project, Workstream: archived, At: s.Now(), Actor: intake.RegistrationActor, Cause: "fixture"}
	_, err = active.Repository.SetFeatureState(ctx, archivedHeader, intake.AbandonedState, "superseded")
	fixturekit.Must(t, err)
	blockArchiveCleanup(t, active.Repository, project, archived, s.Now(), intake.RegistrationActor)
	fixturekit.Must(t, s.RefreshRuntime())
	fixturekit.Must(t, s.Store.SetArchived(runtime.Archive{Project: project, Workstream: archived, ArchivedAt: s.Now(), Title: "Archived fixture workstream", State: intake.AbandonedState}))
	archiveBefore, ok := intakefixture.ArchiveRecord(t, s.Store, archived)
	if !ok {
		t.Fatal("archive record missing before migration")
	}

	documentsBefore := map[string][]byte{}
	for _, name := range []string{"charter.md", trace.EntitiesPath} {
		content, err := fixturekit.ProjectDocument(t, s.Cfg.Root, project, name)
		fixturekit.Must(t, err)
		documentsBefore[name] = content
	}
	buildingBefore := fixturekit.TraceCommits(t, fixturekit.WorkstreamTraceDirectory(t, s.Cfg.Root, project, building))
	secondBefore := fixturekit.TraceCommits(t, fixturekit.WorkstreamTraceDirectory(t, s.Cfg.Root, project, second))
	sessionBefore := fixturekit.TraceCommits(t, fixturekit.WorkstreamTraceDirectory(t, s.Cfg.Root, project, session))
	deliveredBefore := fixturekit.TraceCommits(t, fixturekit.WorkstreamTraceDirectory(t, s.Cfg.Root, project, delivered))
	abandonedBefore := fixturekit.TraceCommits(t, fixturekit.WorkstreamTraceDirectory(t, s.Cfg.Root, project, abandoned))
	archivedBefore := fixturekit.TraceCommits(t, fixturekit.WorkstreamTraceDirectory(t, s.Cfg.Root, project, archived))

	fixturekit.Must(t, s.Close())
	s, c = journey.StartT(t, opts)
	_ = c

	active = s.Sole()
	if active == nil {
		t.Fatal("no active project after restart")
	}
	groups, revision, err := trace.Workgroups(active.Repository)
	fixturekit.Must(t, err)
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

	archiveAfter, ok := intakefixture.ArchiveRecord(t, s.Store, archived)
	if !ok {
		t.Fatal("archive record lost by migration")
	}
	if archiveAfter.Project != archiveBefore.Project || archiveAfter.Workstream != archiveBefore.Workstream ||
		archiveAfter.Title != archiveBefore.Title || archiveAfter.State != archiveBefore.State ||
		archiveAfter.DeliveryLink != archiveBefore.DeliveryLink ||
		!archiveAfter.ArchivedAt.Equal(archiveBefore.ArchivedAt) || !archiveAfter.CleanedAt.Equal(archiveBefore.CleanedAt) {
		t.Fatalf("archive record changed by migration: before=%+v after=%+v", archiveBefore, archiveAfter)
	}
	list, err := active.Repository.Workstreams()
	fixturekit.Must(t, err)
	if !slices.Contains(list, archived) {
		t.Fatal("archived workstream's trace was removed by migration")
	}

	for name, before := range documentsBefore {
		after, err := fixturekit.ProjectDocument(t, s.Cfg.Root, project, name)
		fixturekit.Must(t, err)
		if string(after) != string(before) {
			t.Fatalf("%s changed by migration", name)
		}
	}
	workstreamTraceUnchanged(t, s.Cfg.Root, project, building, buildingBefore)
	workstreamTraceUnchanged(t, s.Cfg.Root, project, second, secondBefore)
	workstreamTraceUnchanged(t, s.Cfg.Root, project, session, sessionBefore)
	workstreamTraceUnchanged(t, s.Cfg.Root, project, delivered, deliveredBefore)
	workstreamTraceUnchanged(t, s.Cfg.Root, project, abandoned, abandonedBefore)
	workstreamTraceUnchanged(t, s.Cfg.Root, project, archived, archivedBefore)

	fixturekit.Must(t, s.Close())
	s, c = journey.StartT(t, opts)
	defer func() { fixturekit.Must(t, s.Close()) }()
	_ = c
	active = s.Sole()
	again, revisionAgain, err := trace.Workgroups(active.Repository)
	fixturekit.Must(t, err)
	if revisionAgain != revision || len(again) != len(groups) {
		t.Fatalf("restart changed workgroups: before=%+v after=%+v", groups, again)
	}
	for _, g := range groups {
		found, ok := trace.WorkgroupOf(again, g.Members[0])
		if !ok || found.ID != g.ID {
			t.Fatalf("restart changed workgroup identity for %s: %+v vs %+v", g.Members[0], g, found)
		}
	}

	unblockArchiveCleanup(t, ctx, active.Repository, archived, s.Now)
	archiveReleased, ok := intakefixture.ArchiveRecord(t, s.Store, archived)
	if !ok {
		t.Fatal("archive record missing after releasing the hold")
	}
	cleanup := &journey.ArchiveCleanup{S: s, Workspaces: &journey.WorkspaceCleanup{Cfg: s.Cfg, Repository: active.Repository, Now: s.Now}}
	eligible, reason, err := cleanup.Prepare(ctx, archiveReleased)
	fixturekit.Must(t, err)
	if !eligible {
		t.Fatalf("archived workstream not eligible for archive cleanup after migration: %s", reason)
	}
}

func TestStartupMigrationResumesAfterAnInjectedCrashWithoutDuplicating(t *testing.T) {
	opts, clone := journey.ProjectFixture(t)
	ctx := context.Background()

	s, c := journey.StartT(t, opts)
	first, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	secondClone := filepath.Join(filepath.Dir(clone), "other")
	fixturekit.DemoGit(t, filepath.Dir(clone), "init", "--quiet", secondClone)
	secondAdded, err := c.AddProject(ctx, api.ProjectAddRequest{Name: "other", Upstream: "upstream/other", Fork: "owner/other", Clone: secondClone})
	fixturekit.Must(t, err)

	var firstActive, secondActive *journey.ActiveProject
	for _, p := range s.ProjectViews() {
		switch p.Id {
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
	fixturekit.Must(t, firstActive.Repository.CreateWorkstream(ctx, firstStream, s.Now(), intake.RegistrationActor))
	fixturekit.Must(t, secondActive.Repository.CreateWorkstream(ctx, secondStream, s.Now(), intake.RegistrationActor))
	fixturekit.Must(t, s.Close())

	lifecycle.EnsureWorkgroupsInterrupt = func(id config.ProjectID) error {
		if id == secondAdded.Project.ID {
			return errors.New("simulated crash mid-migration")
		}
		return nil
	}
	_, err = journey.Start(ctx, opts)
	if err == nil {
		t.Fatal("expected the injected crash to fail startup")
	}
	lifecycle.EnsureWorkgroupsInterrupt = nil

	s, c = journey.StartT(t, opts)
	defer func() { fixturekit.Must(t, s.Close()) }()
	_ = c

	var firstAfter, secondAfter *journey.ActiveProject
	for _, p := range s.ProjectViews() {
		switch p.Id {
		case first.Project.ID:
			firstAfter = p
		case secondAdded.Project.ID:
			secondAfter = p
		}
	}
	if firstAfter == nil || secondAfter == nil {
		t.Fatal("both projects must still be active")
	}
	firstGroups, firstRevision, err := trace.Workgroups(firstAfter.Repository)
	fixturekit.Must(t, err)
	if firstRevision != 1 || len(firstGroups) != 1 {
		t.Fatalf("first project's workgroups after resumed migration: revision=%d groups=%+v", firstRevision, firstGroups)
	}
	if g, ok := trace.WorkgroupOf(firstGroups, firstStream); !ok || len(g.Members) != 1 {
		t.Fatalf("first project's workstream workgroup: %+v ok=%v", g, ok)
	}
	secondGroups, secondRevision, err := trace.Workgroups(secondAfter.Repository)
	fixturekit.Must(t, err)
	if secondRevision != 1 || len(secondGroups) != 1 {
		t.Fatalf("second project's workgroups after resumed migration: revision=%d groups=%+v", secondRevision, secondGroups)
	}
	if g, ok := trace.WorkgroupOf(secondGroups, secondStream); !ok || len(g.Members) != 1 {
		t.Fatalf("second project's workstream workgroup: %+v ok=%v", g, ok)
	}
}

func TestWorkgroupResolutionFiresOnlyAfterEveryMemberIsResolvedThroughALivePass(t *testing.T) {
	t.Parallel()
	opts, clone := journey.ProjectFixture(t)
	var passed atomic.Int64
	opts.SchedulePassed = func() { passed.Add(1) }
	ctx := context.Background()

	s, c := journey.StartT(t, opts)
	defer func() { fixturekit.Must(t, s.Close()) }()
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	project := added.Project.ID

	active := s.Sole()
	if active == nil {
		t.Fatal("no active project")
	}
	first := config.WorkstreamID("w_00000000000000000000000000000031")
	second := config.WorkstreamID("w_00000000000000000000000000000032")
	fixturekit.Must(t, active.Repository.CreateWorkstream(ctx, first, s.Now(), intake.RegistrationActor))
	fixturekit.Must(t, active.Repository.CreateWorkstream(ctx, second, s.Now(), intake.RegistrationActor))
	g, err := trace.CreateWorkgroup(ctx, active.Repository, []config.WorkstreamID{first, second}, s.Now(), intake.RegistrationActor, "test-workgroup")
	fixturekit.Must(t, err)

	deliverFirst := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-first", Revision: 1, Project: project, Workstream: first, At: s.Now(), Actor: intake.RegistrationActor, Cause: "fixture"}
	_, err = active.Repository.SetFeatureState(ctx, deliverFirst, intake.DeliveredState, "pull request merged")
	fixturekit.Must(t, err)
	journey.RecordResolutionMerge(t, active.Repository, first)

	intakefixture.AwaitPasses(t, &passed, 2)
	resolved, _, err := trace.ResolvedWorkgroups(active.Repository)
	fixturekit.Must(t, err)
	if len(resolved) != 0 {
		t.Fatalf("workgroup resolved after only one of its two members delivered: %+v", resolved)
	}
	if _, err := active.Repository.Workstreams(); err != nil {
		t.Fatalf("workstreams unreadable while one member is unresolved: %v", err)
	}

	deliverSecond := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-second", Revision: 1, Project: project, Workstream: second, At: s.Now(), Actor: intake.RegistrationActor, Cause: "fixture"}
	_, err = active.Repository.SetFeatureState(ctx, deliverSecond, intake.DeliveredState, "pull request merged")
	fixturekit.Must(t, err)
	journey.RecordResolutionMerge(t, active.Repository, second)

	intakefixture.AwaitPasses(t, &passed, 2)
	resolved, _, err = trace.ResolvedWorkgroups(active.Repository)
	fixturekit.Must(t, err)
	if len(resolved) != 1 || resolved[0] != g.ID {
		t.Fatalf("workgroup not resolved, exactly once, after both members delivered: %+v, want [%s]", resolved, g.ID)
	}

	streams, err := active.Repository.Workstreams()
	fixturekit.Must(t, err)
	if !slices.Contains(streams, first) || !slices.Contains(streams, second) {
		t.Fatalf("both members' traces must remain readable after the workgroup resolves: %+v", streams)
	}
	for _, stream := range []config.WorkstreamID{first, second} {
		if _, err := trace.Read[trace.Document](active.Repository, stream); err != nil {
			t.Fatalf("workstream %s's trace unreadable after the workgroup resolves: %v", stream, err)
		}
	}

	intakefixture.AwaitPasses(t, &passed, 2)
	resolved, _, err = trace.ResolvedWorkgroups(active.Repository)
	fixturekit.Must(t, err)
	if len(resolved) != 1 {
		t.Fatalf("later passes resolved the workgroup more than once: %+v", resolved)
	}
}

func TestWorkgroupResolutionCountsAMemberArchiveCleanupAlreadyDeletedAsResolved(t *testing.T) {
	t.Parallel()
	opts, clone := journey.ProjectFixture(t)
	ctx := context.Background()
	setupReady := make(chan struct{})
	setupRelease := make(chan struct{})
	gate := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
	t.Cleanup(gate.Release)
	opts.Boundary = gate.Checkpoint
	var passed atomic.Int64
	passChanges := make(chan struct{}, 1)
	opts.SchedulePassed = func() {
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
	s, c := journey.StartT(t, opts)
	t.Cleanup(func() { fixturekit.Must(t, s.Close()) })
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	project := added.Project.ID
	select {
	case <-setupReady:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("live controller did not reach the setup boundary")
	}
	active := s.Sole()
	if active == nil {
		t.Fatal("no active project")
	}
	kept := config.WorkstreamID("w_00000000000000000000000000000041")
	gone := config.WorkstreamID("w_00000000000000000000000000000042")
	fixturekit.Must(t, active.Repository.CreateWorkstream(ctx, kept, s.Now(), intake.RegistrationActor))
	fixturekit.Must(t, active.Repository.CreateWorkstream(ctx, gone, s.Now(), intake.RegistrationActor))
	g, err := trace.CreateWorkgroup(ctx, active.Repository, []config.WorkstreamID{kept, gone}, s.Now(), intake.RegistrationActor, "test-workgroup")
	fixturekit.Must(t, err)
	deliverGone := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-gone", Revision: 1, Project: project, Workstream: gone, At: s.Now(), Actor: intake.RegistrationActor, Cause: "fixture"}
	_, err = active.Repository.SetFeatureState(ctx, deliverGone, intake.DeliveredState, "pull request merged")
	fixturekit.Must(t, err)
	journey.RecordResolutionMerge(t, active.Repository, gone)
	fixturekit.Must(t, s.RefreshRuntime())
	_, err = c.Archive(ctx, gone)
	fixturekit.Must(t, err)
	close(setupRelease)
	gate.Await(t)
	gate.Release()
	changes, closeChanges := journey.ServiceChanges(t, s)
	defer closeChanges()
	await := func(phase string, ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(fixturekit.DemoTimeout)
		for !ready() {
			if time.Now().After(deadline) {
				t.Fatalf("live controller did not complete %s", phase)
			}
			changes()
		}
	}
	await("owner-authorized archive cleanup", func() bool {
		a, found := intakefixture.ArchiveRecord(t, s.Store, gone)
		return found && a.Resolved && !a.CleanupStartedAt.IsZero() && !a.CleanedAt.IsZero()
	})
	streams, err := active.Repository.Workstreams()
	fixturekit.Must(t, err)
	if slices.Contains(streams, gone) {
		t.Fatalf("gone's trace must be fully deleted for this test: %+v", streams)
	}
	resolved, _, err := trace.ResolvedWorkgroups(active.Repository)
	fixturekit.Must(t, err)
	if len(resolved) != 0 {
		t.Fatalf("workgroup resolved while its other member is still unresolved: %+v", resolved)
	}
	deliverKept := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-kept", Revision: 1, Project: project, Workstream: kept, At: s.Now(), Actor: intake.RegistrationActor, Cause: "fixture"}
	_, err = active.Repository.SetFeatureState(ctx, deliverKept, intake.DeliveredState, "pull request merged")
	fixturekit.Must(t, err)
	journey.RecordResolutionMerge(t, active.Repository, kept)
	await("final group resolution", func() bool {
		resolved, _, err = trace.ResolvedWorkgroups(active.Repository)
		fixturekit.Must(t, err)
		return len(resolved) != 0
	})
	if len(resolved) != 1 || resolved[0] != g.ID {
		t.Fatalf("workgroup not resolved, exactly once, once its remaining member is delivered: %+v, want [%s]", resolved, g.ID)
	}
	target := passed.Load() + 2
	deadline := time.NewTimer(fixturekit.DemoTimeout)
	defer deadline.Stop()
	for passed.Load() < target {
		select {
		case <-passChanges:
		case <-deadline.C:
			t.Fatal("live controller did not finish later passes")
		}
	}
	resolved, _, err = trace.ResolvedWorkgroups(active.Repository)
	fixturekit.Must(t, err)
	if len(resolved) != 1 {
		t.Fatalf("later passes resolved the workgroup more than once: %+v", resolved)
	}
}

func TestCrossRepoWorkgroupResolutionFiresOnlyAfterEveryMemberIsResolved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, clone1 := journey.ProjectFixture(t)
	home := filepath.Dir(clone1)
	clone2 := filepath.Join(home, "other")
	fixturekit.DemoGit(t, home, "init", "--quiet", clone2)
	var passed atomic.Int64
	opts.SchedulePassed = func() { passed.Add(1) }

	s, c := journey.StartT(t, opts)
	added1, err := c.AddProject(ctx, fixturekit.Request(clone1))
	fixturekit.Must(t, err)
	added2, err := c.AddProject(ctx, api.ProjectAddRequest{Name: "other", Upstream: "upstream/other", Fork: "owner/other", Clone: clone2})
	fixturekit.Must(t, err)
	_, active1 := s.RuntimeOf(added1.Project.ID)
	_, active2 := s.RuntimeOf(added2.Project.ID)
	if active1 == nil || active2 == nil {
		t.Fatal("both projects must be active")
	}

	first := config.WorkstreamID("w_00000000000000000000000000000061")
	second := config.WorkstreamID("w_00000000000000000000000000000062")
	fixturekit.Must(t, active1.Repository.CreateWorkstream(ctx, first, s.Now(), intake.RegistrationActor))
	fixturekit.Must(t, active2.Repository.CreateWorkstream(ctx, second, s.Now(), intake.RegistrationActor))

	id, apiFault := s.Lifecycle().DraftWorkgroup(ctx, []config.WorkstreamID{first, second})
	if apiFault != nil {
		t.Fatalf("draft workgroup: %v", apiFault)
	}

	resolvedIDs := func() []config.WorkgroupID {
		t.Helper()
		members := []trace.CrossRepoMember{{Workstream: first, Project: active1.Repository}, {Workstream: second, Project: active2.Repository}}
		wt, err := trace.OpenWorkgroupTrace(s.Current().Root, id, members)
		fixturekit.Must(t, err)
		defer wt.Close()
		ids, _, err := trace.ResolvedWorkgroups(wt)
		fixturekit.Must(t, err)
		return ids
	}

	intakefixture.AwaitPasses(t, &passed, 2)
	if ids := resolvedIDs(); len(ids) != 0 {
		t.Fatalf("workgroup resolved before either member delivered: %+v", ids)
	}

	deliverFirst := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-first", Revision: 1, Project: added1.Project.ID, Workstream: first, At: s.Now(), Actor: intake.RegistrationActor, Cause: "fixture"}
	_, err = active1.Repository.SetFeatureState(ctx, deliverFirst, intake.DeliveredState, "pull request merged")
	fixturekit.Must(t, err)
	journey.RecordResolutionMerge(t, active1.Repository, first)

	intakefixture.AwaitPasses(t, &passed, 4)
	if ids := resolvedIDs(); len(ids) != 0 {
		t.Fatalf("workgroup resolved after only one of its two members delivered: %+v", ids)
	}
	if _, err := active1.Repository.Workstreams(); err != nil {
		t.Fatalf("workstreams unreadable while one member is unresolved: %v", err)
	}

	deliverSecond := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver-second", Revision: 1, Project: added2.Project.ID, Workstream: second, At: s.Now(), Actor: intake.RegistrationActor, Cause: "fixture"}
	_, err = active2.Repository.SetFeatureState(ctx, deliverSecond, intake.DeliveredState, "pull request merged")
	fixturekit.Must(t, err)
	journey.RecordResolutionMerge(t, active2.Repository, second)

	intakefixture.AwaitPasses(t, &passed, 4)
	ids := resolvedIDs()
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("workgroup not resolved, exactly once, after both members delivered: %+v, want [%s]", ids, id)
	}

	intakefixture.AwaitPasses(t, &passed, 4)
	if ids := resolvedIDs(); len(ids) != 1 {
		t.Fatalf("later passes resolved the workgroup more than once: %+v", ids)
	}
}
