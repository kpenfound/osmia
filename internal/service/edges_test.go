package service

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// edgeGraphFixture is a stopped, single-project service with two
// workstreams connected by a single workgroup graph edge, ready for the
// edge-resolution tests below. The service is stopped, as the same-repo
// base-dependency tests it mirrors are (base_upstream_test.go), so that
// nothing but the test itself drives edgeResolverPass and edgeWaitPass: a
// live reconcile loop would otherwise run them concurrently on its own
// schedule and race the test's own calls. It is the "test fixture...
// records the project spec, graph and slices" workgroup-spec's own guidance
// sanctions (plan#workgroup-spec), reused here since edge resolution only
// reads what CreateWorkgroup and RecordProjectSpec already record.
type edgeGraphFixture struct {
	s          *Service
	clock      *demoClock
	project    config.ProjectID
	repository *trace.Repository
	upstream   config.WorkstreamID
	downstream config.WorkstreamID
}

func newEdgeGraphFixture(t *testing.T, edgeType string) *edgeGraphFixture {
	t.Helper()
	f := newDebateFixture(t, 1, 1)
	f.stop(t)
	ctx := context.Background()
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { repository.Close() })
	upstream := config.WorkstreamID("w_00000000000000000000000000000a01")
	downstream := config.WorkstreamID("w_00000000000000000000000000000a02")
	must(t, repository.CreateWorkstream(ctx, upstream, f.s.now(), registrationActor))
	must(t, repository.CreateWorkstream(ctx, downstream, f.s.now(), registrationActor))
	g, err := trace.CreateWorkgroup(ctx, repository, []config.WorkstreamID{upstream, downstream}, f.s.now(), ownerActor, "fixture")
	must(t, err)
	spec := trace.ProjectSpec{Behavior: "Downstream depends on upstream.", Graph: []trace.Edge{{From: upstream, To: downstream, Type: edgeType}}}
	must(t, trace.RecordProjectSpec(ctx, repository, g, spec, f.s.now(), ownerActor, "fixture"))
	return &edgeGraphFixture{s: f.s, clock: f.clock, project: f.project, repository: repository, upstream: upstream, downstream: downstream}
}

// resolvePasses runs both edge passes once, the way the reconcile controller
// would on its own schedule: edgeResolverPass refreshes the observation of
// every incoming edge, and edgeWaitPass keeps edgeWaitSubject current from
// it. It advances the fixture's clock well past edgeResolverPass's configured polling
// interval first, so each call sees the latest recorded state
// rather than skipping it as a too-recent repeat.
func (f *edgeGraphFixture) resolvePasses(t *testing.T) {
	t.Helper()
	f.clock.mu.Lock()
	f.clock.now = f.clock.now.Add(11 * time.Minute)
	f.clock.mu.Unlock()
	must(t, f.s.edgeResolverPass(context.Background(), f.repository))
	must(t, f.s.edgeWaitPass(context.Background(), f.repository))
}

// holdsMason reports whether the dispatch gate declines a mason turn of
// stream, the way it does for real candidates in production.
func (f *edgeGraphFixture) holdsMason(t *testing.T, stream config.WorkstreamID) bool {
	t.Helper()
	held, err := f.s.holds(f.project, librarianWorkstream(f.project), f.repository, scheduler.Candidate{Workstream: stream, Thread: trace.Thread{Identity: trace.Agent{Role: masonRole}}})
	must(t, err)
	return held
}

// inboxLen is the number of entries ever escalated to the owner.
func (f *edgeGraphFixture) inboxLen(t *testing.T) int {
	t.Helper()
	entries, err := f.repository.Inbox()
	must(t, err)
	return len(entries)
}

// recordOwnerMerge records, directly on upstream's own trace, the owner's
// merge of its feature branch into upstream outside the factory: the only
// other recorded event (besides a detected pull request merge) that may
// resolve a hard_block or merge_order edge (spec#11).
func (f *edgeGraphFixture) recordOwnerMerge(t *testing.T, stream config.WorkstreamID, commit string) {
	t.Helper()
	ctx := context.Background()
	feature, err := f.repository.Workflow(stream, trace.FeatureSubject)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver", Revision: 1, Project: f.repository.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: "fixture"}
	_, err = f.repository.MoveFeatureState(ctx, h, feature.Value, DeliveredState, "fixture delivery")
	must(t, err)
	record := DeliveryMerge{Branch: featureBranch(stream), Commit: commit, Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: commit}}
	data, err := json.Marshal(record)
	must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: mergeDocument, Revision: 1, Project: f.repository.Project(), Workstream: stream, At: f.s.now(), Actor: ownerActor, Cause: "fixture"}, Path: mergePath, Content: string(data)}
	must(t, f.repository.RecordDocuments(ctx, []trace.Document{doc}))
}

// TestEdgeGraphRefusesACycle covers the "a cyclic graph is refused" half of
// acceptance item 1. Cycle validation belongs to unit workgroup-spec
// (trace.ValidateGraph); this unit's own edge resolution depends on it
// never recording a cyclic graph, so it is exercised directly here too.
func TestEdgeGraphRefusesACycle(t *testing.T) {
	t.Parallel()
	a := config.WorkstreamID("w_00000000000000000000000000000b01")
	b := config.WorkstreamID("w_00000000000000000000000000000b02")
	err := trace.ValidateGraph([]config.WorkstreamID{a, b}, []trace.Edge{
		{From: a, To: b, Type: trace.EdgeHardBlock},
		{From: b, To: a, Type: trace.EdgeMergeOrder},
	})
	if err == nil {
		t.Fatal("a cyclic graph was not refused")
	}
}

// TestContractEdgeNeverGatesOrWaits covers that a contract edge resolves the
// moment the project spec carrying it is ratified: it never appears in the
// incoming merge-edge set a hard block or merge order would, so it never
// holds scheduling and never shows a wait, with nothing further to observe
// or poll (spec#11).
func TestContractEdgeNeverGatesOrWaits(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeContract)
	pull := &fakePulls{}
	f.s.options.PullRequests = pull
	recordUpstreamOwnPullRequest(t, f.s, f.repository, f.upstream, DeliveryPublication{Status: publicationOpened, Upstream: "owner/repo-a", Fork: "owner/repo-a-fork", Branch: featureBranch(f.upstream), PullRequest: 123})
	if f.holdsMason(t, f.downstream) {
		t.Fatal("a contract edge held the dependent's mason turn")
	}
	if wait, err := edgeWaitDisplay(f.repository, f.downstream); err != nil || wait != "" {
		t.Fatalf("contract edge showed a wait: %q, %v", wait, err)
	}
	f.resolvePasses(t)
	if pull.finds != 0 {
		t.Fatalf("edge reconciliation polled a published workstream without merge edges %d times", pull.finds)
	}
	if f.holdsMason(t, f.downstream) {
		t.Fatal("a contract edge held the dependent's mason turn after resolution passes ran")
	}
	if n := f.inboxLen(t); n != 0 {
		t.Fatalf("contract edge created %d inbox entries", n)
	}
}

// TestHardBlockEdgeHoldsNoUnitUntilRecordedMergeNeverOnAgentOutcome covers
// acceptance item 1's "hard block" half and the "never on an agent outcome"
// requirement together: the dependent's mason turns stay held, and the wait
// status stays shown, through an upstream workstream that merely reaches
// AssembledState (an agent-visible outcome with no recorded merge), and only
// clear once the owner's own recorded merge exists (spec#11, spec#15).
func TestHardBlockEdgeHoldsNoUnitUntilRecordedMergeNeverOnAgentOutcome(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	ctx := context.Background()

	f.resolvePasses(t)
	if !f.holdsMason(t, f.downstream) {
		t.Fatal("a hard-blocked dependent's mason turn was not held before the upstream merged")
	}
	if wait, err := edgeWaitDisplay(f.repository, f.downstream); err != nil || wait != edgeWaitMessage(string(f.upstream)) {
		t.Fatalf("wait message = %q, %v; want %q", wait, err, edgeWaitMessage(string(f.upstream)))
	}

	// An agent outcome alone, with no recorded merge, changes nothing: the
	// upstream workstream reaching AssembledState is exactly what a mason or
	// reviewer could cause by finishing its own work, never a merge.
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "assemble", Revision: 1, Project: f.repository.Project(), Workstream: f.upstream, At: f.s.now(), Actor: foremanActor, Cause: "fixture"}
	_, err := f.repository.SetFeatureState(ctx, h, AssembledState, "fixture assembly")
	must(t, err)
	f.resolvePasses(t)
	if !f.holdsMason(t, f.downstream) {
		t.Fatal("an agent outcome with no recorded merge released the hard-blocked dependent")
	}
	if n := f.inboxLen(t); n != 0 {
		t.Fatalf("waiting on a hard-blocked edge created %d inbox entries", n)
	}

	f.recordOwnerMerge(t, f.upstream, "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2")
	f.resolvePasses(t)
	if f.holdsMason(t, f.downstream) {
		t.Fatal("the hard-blocked dependent's mason turn stayed held after the owner recorded the merge")
	}
	if wait, err := edgeWaitDisplay(f.repository, f.downstream); err != nil || wait != "" {
		t.Fatalf("wait message after merge = %q, %v; want none", wait, err)
	}
	if n := f.inboxLen(t); n != 0 {
		t.Fatalf("the hard-blocked edge's whole wait and resolution created %d inbox entries", n)
	}
}

// TestMergeOrderEdgeLetsBothBuildButHoldsReadinessUntilMerge covers that a
// merge-order edge, unlike hard block, never holds the dependent's own
// scheduling (both workstreams build in parallel), gives the dependent's
// roles the upstream's own feature branch to build against while
// unresolved, and still reports an unresolved wait and keeps the dependent's
// pull request from being marked ready until the upstream merges (spec#11,
// spec#12).
func TestMergeOrderEdgeLetsBothBuildButHoldsReadinessUntilMerge(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeMergeOrder)
	f.resolvePasses(t)

	if f.holdsMason(t, f.downstream) {
		t.Fatal("a merge-order edge held the dependent's own mason turn; both workstreams must build in parallel")
	}
	if reference, waiting, err := unresolvedMergeOrderEdge(f.repository, f.downstream); err != nil || !waiting || reference != string(f.upstream) {
		t.Fatalf("unresolvedMergeOrderEdge = %q, %v, %v; want (%q, true, nil)", reference, waiting, err, f.upstream)
	}
	if wait, err := edgeWaitDisplay(f.repository, f.downstream); err != nil || wait == "" {
		t.Fatalf("merge-order edge showed no wait before the upstream merged: %q, %v", wait, err)
	}
	observed, _, _, err := edgeObservationAt(f.repository, f.downstream)
	must(t, err)
	if len(observed.Edges) != 1 || observed.Edges[0].Merged || observed.Edges[0].BuildBranch == "" {
		t.Fatalf("edge observation = %+v; want one unmerged edge naming a build branch", observed.Edges)
	}

	f.recordOwnerMerge(t, f.upstream, "b1c2d3e4f5a6b1c2d3e4f5a6b1c2d3e4f5a6b1c2")
	f.resolvePasses(t)
	if reference, waiting, err := unresolvedMergeOrderEdge(f.repository, f.downstream); err != nil || waiting {
		t.Fatalf("unresolvedMergeOrderEdge after merge = %q, %v, %v; want resolved", reference, waiting, err)
	}
	if resolved, err := workgroupMergeOrderResolved(f.repository, f.downstream); err != nil || resolved == "" {
		t.Fatalf("workgroupMergeOrderResolved after merge = %q, %v; want a non-empty summary", resolved, err)
	}
	if n := f.inboxLen(t); n != 0 {
		t.Fatalf("the merge-order edge's whole wait and resolution created %d inbox entries", n)
	}
}

// TestEdgeResolverPassPollsGitHubAndNeverResolvesOnAnAgentOutcome shows the
// other recorded event edges may resolve on: a GitHub poll finding the
// upstream's own published pull request merged, through the pulls.Client
// the service is configured with, and never from the upstream merely
// reaching DeliveredState (an agent-driven publish) on its own (spec#11).
func TestEdgeResolverPassPollsGitHubAndNeverResolvesOnAnAgentOutcome(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeMergeOrder)
	ctx := context.Background()

	publication := DeliveryPublication{Status: publicationOpened, Upstream: "owner/dagger", Fork: "owner/dagger-fork", Branch: featureBranch(f.upstream), Commit: "c1d2e3f4a5b6c1d2e3f4a5b6c1d2e3f4a5b6c1d2", PullRequest: 42, URL: "https://example.invalid/pull/42"}
	data, err := json.Marshal(publication)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: publicationDocument, Revision: 1, Project: f.repository.Project(), Workstream: f.upstream, At: f.s.now(), Actor: foremanActor, Cause: "fixture"}
	must(t, f.repository.RecordDocuments(ctx, []trace.Document{{Header: h, Path: publicationPath, Content: string(data)}}))
	deliver := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver", Revision: 1, Project: f.repository.Project(), Workstream: f.upstream, At: f.s.now(), Actor: foremanActor, Cause: "fixture"}
	_, err = f.repository.SetFeatureState(ctx, deliver, DeliveredState, "fixture delivery")
	must(t, err)

	fake := &fakePulls{prs: []pulls.PullRequest{{Number: publication.PullRequest, URL: publication.URL, State: "open", HeadRepository: publication.Fork, Head: publication.Branch, HeadCommit: publication.Commit, MergeCommit: "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12", Base: "main"}}}
	f.s.mu.Lock()
	f.s.options.PullRequests = fake
	f.s.mu.Unlock()

	// The pull request is open, not merged: an agent-driven delivery (the
	// publish that opened it) is not itself a recorded merge.
	f.resolvePasses(t)
	if reference, waiting, err := unresolvedMergeOrderEdge(f.repository, f.downstream); err != nil || !waiting || reference != "owner/dagger#42" {
		t.Fatalf("unresolvedMergeOrderEdge with an open pull request = %q, %v, %v; want (\"owner/dagger#42\", true, nil)", reference, waiting, err)
	}

	fake.mu.Lock()
	fake.prs[0].Merged = true
	fake.mu.Unlock()
	f.resolvePasses(t)
	if reference, waiting, err := unresolvedMergeOrderEdge(f.repository, f.downstream); err != nil || waiting {
		t.Fatalf("unresolvedMergeOrderEdge after the poll found it merged = %q, %v, %v; want resolved", reference, waiting, err)
	}
	observed, _, _, err := edgeObservationAt(f.repository, f.downstream)
	must(t, err)
	if observed.Edges[0].Commit != fake.prs[0].MergeCommit || observed.Edges[0].Commit == publication.Commit {
		t.Fatalf("dependency pins published head instead of merged commit: %+v", observed.Edges)
	}
}

func TestHardBlockedBuildStartsNoUnitBeforeMerge(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	base := f.upstream(t)
	downstream, repository := seedBuild(t, f, "hard-block", validPlan, config.WorkspacesGit, base)
	ctx := context.Background()
	upstream := config.WorkstreamID("w_00000000000000000000000000000b01")
	must(t, repository.CreateWorkstream(ctx, upstream, f.s.now(), registrationActor))
	group, err := trace.CreateWorkgroup(ctx, repository, []config.WorkstreamID{upstream, downstream}, f.s.now(), ownerActor, "fixture")
	must(t, err)
	must(t, trace.RecordProjectSpec(ctx, repository, group, trace.ProjectSpec{Behavior: "Wait for the upstream merge.", Graph: []trace.Edge{{From: upstream, To: downstream, Type: trace.EdgeHardBlock}}}, f.s.now(), ownerActor, "fixture"))
	build := &masons{s: f.s, cfg: f.s.about(repository), repository: repository}
	before, err := repository.WorkflowStates(downstream)
	must(t, err)
	must(t, build.Pass(ctx))
	states, err := repository.WorkflowStates(downstream)
	must(t, err)
	for _, unit := range []string{"resume", "dedupe"} {
		if !reflect.DeepEqual(states[trace.UnitSubject(unit)], before[trace.UnitSubject(unit)]) {
			t.Fatalf("hard-blocked unit %s started before even the first observation: %+v", unit, states)
		}
	}
	for _, unit := range []string{"resume", "dedupe"} {
		threads, err := repository.Threads(downstream)
		must(t, err)
		for _, thread := range threads {
			if thread.Identity.Unit == unit && thread.Identity.Role == masonRole {
				t.Fatal("hard block queued a mason thread")
			}
		}
	}
}

func TestEdgeWaitIsStatusWithoutInboxOrWebhookNotification(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeMergeOrder)
	f.s.setSole(&activeProject{id: f.project, repository: f.repository})
	hook := newFakeWebhook(t)
	f.s.cfg.Notify.Webhook = hook.hook()
	notifications := newNotifier(f.s, time.Second, nil)
	notifications.pass(context.Background())
	recordUpstreamOwnPullRequest(t, f.s, f.repository, f.upstream, DeliveryPublication{Status: publicationOpened, Upstream: "owner/repo-a", Fork: "owner/repo-a-fork", Branch: featureBranch(f.upstream), PullRequest: 123})
	f.s.options.PullRequests = &fakePulls{prs: []pulls.PullRequest{{Number: 123, State: "open", HeadRepository: "owner/repo-a-fork", Head: featureBranch(f.upstream), Base: "main"}}}
	f.resolvePasses(t)
	statuses, _, api := f.s.statuses()
	if api != nil {
		t.Fatal(api)
	}
	data, err := json.Marshal(statuses)
	must(t, err)
	if !strings.Contains(string(data), `"wait":"waiting on owner: owner/repo-a#123 merge"`) {
		t.Fatalf("status API omits the merge wait: %s", data)
	}
	inbox, api := f.s.inbox(context.Background())
	if api != nil || len(inbox.Entries) != 0 {
		t.Fatalf("edge wait created owner inbox entries: %+v, %v", inbox, api)
	}
	notifications.pass(context.Background())
	if len(hook.received()) != 0 {
		t.Fatal("edge wait sent a webhook notification")
	}
}
