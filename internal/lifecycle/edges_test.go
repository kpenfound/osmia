package lifecycle_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type edgeGraphFixture struct {
	s          *journey.Service
	clock      *fixturekit.DemoClock
	project    config.ProjectID
	repository *trace.Repository
	upstream   config.WorkstreamID
	downstream config.WorkstreamID
}

func newEdgeGraphFixture(t *testing.T, edgeType string) *edgeGraphFixture {
	t.Helper()
	f := journey.NewDebateFixture(t, 1, 1)
	f.Stop(t)
	repository, upstream, downstream := fixturekit.EdgeGraph(t, f.S.Cfg.Root, f.S.Cfg.Project, edgeType, f.S.Now(), intake.RegistrationActor, intake.OwnerActor)
	return &edgeGraphFixture{s: f.S, clock: f.Clock, project: f.Project, repository: repository, upstream: upstream, downstream: downstream}
}

func (f *edgeGraphFixture) resolvePasses(t *testing.T) {
	t.Helper()
	f.clock.Mu.Lock()
	f.clock.At = f.clock.At.Add(11 * time.Minute)
	f.clock.Mu.Unlock()
	fixturekit.Must(t, f.s.Lifecycle().EdgeResolverPass(context.Background(), f.repository))
	fixturekit.Must(t, f.s.Lifecycle().EdgeWaitPass(context.Background(), f.repository))
}

func (f *edgeGraphFixture) holdsMason(t *testing.T, stream config.WorkstreamID) bool {
	t.Helper()
	held, err := f.s.Holds(f.project, f.repository, scheduler.Candidate{Workstream: stream, Thread: trace.Thread{Identity: trace.Agent{Role: intake.MasonRole}}})
	fixturekit.Must(t, err)
	return held
}

func (f *edgeGraphFixture) inboxLen(t *testing.T) int {
	t.Helper()
	entries, err := f.repository.Inbox()
	fixturekit.Must(t, err)
	return len(entries)
}

func (f *edgeGraphFixture) recordOwnerMerge(t *testing.T, stream config.WorkstreamID, commit string) {
	t.Helper()
	ctx := context.Background()
	feature, err := f.repository.Workflow(stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver", Revision: 1, Project: f.repository.Project(), Workstream: stream, At: f.s.Now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = f.repository.MoveFeatureState(ctx, h, feature.Value, intake.DeliveredState, "fixture delivery")
	fixturekit.Must(t, err)
	record := api.DeliveryMerge{Branch: intake.FeatureBranch(stream), Commit: commit, Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: commit}}
	data, err := json.Marshal(record)
	fixturekit.Must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.MergeDocument, Revision: 1, Project: f.repository.Project(), Workstream: stream, At: f.s.Now(), Actor: intake.OwnerActor, Cause: "fixture"}, Path: delivery.MergePath, Content: string(data)}
	fixturekit.Must(t, f.repository.RecordDocuments(ctx, []trace.Document{doc}))
}

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

func TestContractEdgeNeverGatesOrWaits(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeContract)
	pull := &fixturekit.FakePulls{}
	f.s.Options.PullRequests = pull
	journey.RecordUpstreamOwnPullRequest(t, f.s, f.repository, f.upstream, api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "owner/repo-a", Fork: "owner/repo-a-fork", Branch: intake.FeatureBranch(f.upstream), PullRequest: 123})
	if f.holdsMason(t, f.downstream) {
		t.Fatal("a contract edge held the dependent's mason turn")
	}
	if wait, err := lifecycle.EdgeWaitDisplay(f.repository, f.downstream); err != nil || wait != "" {
		t.Fatalf("contract edge showed a wait: %q, %v", wait, err)
	}
	f.resolvePasses(t)
	if pull.Finds != 0 {
		t.Fatalf("edge reconciliation polled a published workstream without merge edges %d times", pull.Finds)
	}
	if f.holdsMason(t, f.downstream) {
		t.Fatal("a contract edge held the dependent's mason turn after resolution passes ran")
	}
	if n := f.inboxLen(t); n != 0 {
		t.Fatalf("contract edge created %d inbox entries", n)
	}
}

func TestHardBlockEdgeHoldsNoUnitUntilRecordedMergeNeverOnAgentOutcome(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
	ctx := context.Background()

	f.resolvePasses(t)
	if !f.holdsMason(t, f.downstream) {
		t.Fatal("a hard-blocked dependent's mason turn was not held before the upstream merged")
	}
	if wait, err := lifecycle.EdgeWaitDisplay(f.repository, f.downstream); err != nil || wait != lifecycle.EdgeWaitMessage(string(f.upstream)) {
		t.Fatalf("wait message = %q, %v; want %q", wait, err, lifecycle.EdgeWaitMessage(string(f.upstream)))
	}

	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "assemble", Revision: 1, Project: f.repository.Project(), Workstream: f.upstream, At: f.s.Now(), Actor: execution.ForemanActor, Cause: "fixture"}
	_, err := f.repository.SetFeatureState(ctx, h, intake.AssembledState, "fixture assembly")
	fixturekit.Must(t, err)
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
	if wait, err := lifecycle.EdgeWaitDisplay(f.repository, f.downstream); err != nil || wait != "" {
		t.Fatalf("wait message after merge = %q, %v; want none", wait, err)
	}
	if n := f.inboxLen(t); n != 0 {
		t.Fatalf("the hard-blocked edge's whole wait and resolution created %d inbox entries", n)
	}
}

func TestMergeOrderEdgeLetsBothBuildButHoldsReadinessUntilMerge(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeMergeOrder)
	f.resolvePasses(t)

	if f.holdsMason(t, f.downstream) {
		t.Fatal("a merge-order edge held the dependent's own mason turn; both workstreams must build in parallel")
	}
	if reference, waiting, err := lifecycle.UnresolvedMergeOrderEdge(f.repository, f.downstream); err != nil || !waiting || reference != string(f.upstream) {
		t.Fatalf("unresolvedMergeOrderEdge = %q, %v, %v; want (%q, true, nil)", reference, waiting, err, f.upstream)
	}
	if wait, err := lifecycle.EdgeWaitDisplay(f.repository, f.downstream); err != nil || wait == "" {
		t.Fatalf("merge-order edge showed no wait before the upstream merged: %q, %v", wait, err)
	}
	observed, _, _, err := lifecycle.EdgeObservationAt(f.repository, f.downstream)
	fixturekit.Must(t, err)
	if len(observed.Edges) != 1 || observed.Edges[0].Merged || observed.Edges[0].BuildBranch == "" {
		t.Fatalf("edge observation = %+v; want one unmerged edge naming a build branch", observed.Edges)
	}

	f.recordOwnerMerge(t, f.upstream, "b1c2d3e4f5a6b1c2d3e4f5a6b1c2d3e4f5a6b1c2")
	f.resolvePasses(t)
	if reference, waiting, err := lifecycle.UnresolvedMergeOrderEdge(f.repository, f.downstream); err != nil || waiting {
		t.Fatalf("unresolvedMergeOrderEdge after merge = %q, %v, %v; want resolved", reference, waiting, err)
	}
	if resolved, err := lifecycle.WorkgroupMergeOrderResolved(f.repository, f.downstream); err != nil || resolved == "" {
		t.Fatalf("workgroupMergeOrderResolved after merge = %q, %v; want a non-empty summary", resolved, err)
	}
	if n := f.inboxLen(t); n != 0 {
		t.Fatalf("the merge-order edge's whole wait and resolution created %d inbox entries", n)
	}
}

func TestEdgeResolverPassPollsGitHubAndNeverResolvesOnAnAgentOutcome(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeMergeOrder)
	ctx := context.Background()

	publication := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "owner/dagger", Fork: "owner/dagger-fork", Branch: intake.FeatureBranch(f.upstream), Commit: "c1d2e3f4a5b6c1d2e3f4a5b6c1d2e3f4a5b6c1d2", PullRequest: 42, URL: "https://example.invalid/pull/42"}
	data, err := json.Marshal(publication)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.PublicationDocument, Revision: 1, Project: f.repository.Project(), Workstream: f.upstream, At: f.s.Now(), Actor: execution.ForemanActor, Cause: "fixture"}
	fixturekit.Must(t, f.repository.RecordDocuments(ctx, []trace.Document{{Header: h, Path: delivery.PublicationPath, Content: string(data)}}))
	deliver := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver", Revision: 1, Project: f.repository.Project(), Workstream: f.upstream, At: f.s.Now(), Actor: execution.ForemanActor, Cause: "fixture"}
	_, err = f.repository.SetFeatureState(ctx, deliver, intake.DeliveredState, "fixture delivery")
	fixturekit.Must(t, err)

	fake := &fixturekit.FakePulls{Prs: []pulls.PullRequest{{Number: publication.PullRequest, URL: publication.URL, State: "open", HeadRepository: publication.Fork, Head: publication.Branch, HeadCommit: publication.Commit, MergeCommit: "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12", Base: "main"}}}
	f.s.Mu.Lock()
	f.s.Options.PullRequests = fake
	f.s.Mu.Unlock()

	f.resolvePasses(t)
	if reference, waiting, err := lifecycle.UnresolvedMergeOrderEdge(f.repository, f.downstream); err != nil || !waiting || reference != "owner/dagger#42" {
		t.Fatalf("unresolvedMergeOrderEdge with an open pull request = %q, %v, %v; want (\"owner/dagger#42\", true, nil)", reference, waiting, err)
	}

	fake.Mu.Lock()
	fake.Prs[0].Merged = true
	fake.Mu.Unlock()
	f.resolvePasses(t)
	if reference, waiting, err := lifecycle.UnresolvedMergeOrderEdge(f.repository, f.downstream); err != nil || waiting {
		t.Fatalf("unresolvedMergeOrderEdge after the poll found it merged = %q, %v, %v; want resolved", reference, waiting, err)
	}
	observed, _, _, err := lifecycle.EdgeObservationAt(f.repository, f.downstream)
	fixturekit.Must(t, err)
	if observed.Edges[0].Commit != fake.Prs[0].MergeCommit || observed.Edges[0].Commit == publication.Commit {
		t.Fatalf("dependency pins published head instead of merged commit: %+v", observed.Edges)
	}
}
func TestHardBlockedBuildStartsNoUnitBeforeMerge(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	base := f.Upstream(t)
	downstream, repository := journey.SeedBuild(t, f, "hard-block", fixturekit.ValidPlan, config.WorkspacesGit, base)
	ctx := context.Background()
	upstream := config.WorkstreamID("w_00000000000000000000000000000b01")
	fixturekit.Must(t, repository.CreateWorkstream(ctx, upstream, f.S.Now(), intake.RegistrationActor))
	group, err := trace.CreateWorkgroup(ctx, repository, []config.WorkstreamID{upstream, downstream}, f.S.Now(), intake.OwnerActor, "fixture")
	fixturekit.Must(t, err)
	fixturekit.Must(t, trace.RecordProjectSpec(ctx, repository, group, trace.ProjectSpec{Behavior: "Wait for the upstream merge.", Graph: []trace.Edge{{From: upstream, To: downstream, Type: trace.EdgeHardBlock}}}, f.S.Now(), intake.OwnerActor, "fixture"))
	build := &journey.Masons{S: f.S, Cfg: f.S.About(repository), Repository: repository}
	before, err := repository.WorkflowStates(downstream)
	fixturekit.Must(t, err)
	fixturekit.Must(t, build.Pass(ctx))
	states, err := repository.WorkflowStates(downstream)
	fixturekit.Must(t, err)
	for _, unit := range []string{"resume", "dedupe"} {
		if !reflect.DeepEqual(states[trace.UnitSubject(unit)], before[trace.UnitSubject(unit)]) {
			t.Fatalf("hard-blocked unit %s started before even the first observation: %+v", unit, states)
		}
	}
	for _, unit := range []string{"resume", "dedupe"} {
		threads, err := repository.Threads(downstream)
		fixturekit.Must(t, err)
		for _, thread := range threads {
			if thread.Identity.Unit == unit && thread.Identity.Role == intake.MasonRole {
				t.Fatal("hard block queued a mason thread")
			}
		}
	}
}
func TestEdgeWaitIsStatusWithoutInboxOrWebhookNotification(t *testing.T) {
	t.Parallel()
	f := newEdgeGraphFixture(t, trace.EdgeMergeOrder)
	f.s.SetSole(&journey.ActiveProject{Id: f.project, Repository: f.repository})
	hook := fixturekit.NewWebhook(t)
	f.s.Cfg.Notify.Webhook = hook.Hook()
	notifications := journey.NewNotifier(f.s, time.Second)
	notifications.Pass(context.Background())
	journey.RecordUpstreamOwnPullRequest(t, f.s, f.repository, f.upstream, api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "owner/repo-a", Fork: "owner/repo-a-fork", Branch: intake.FeatureBranch(f.upstream), PullRequest: 123})
	f.s.Options.PullRequests = &fixturekit.FakePulls{Prs: []pulls.PullRequest{{Number: 123, State: "open", HeadRepository: "owner/repo-a-fork", Head: intake.FeatureBranch(f.upstream), Base: "main"}}}
	f.resolvePasses(t)
	statuses, _, apiFault := journey.InspectForTest(f.s).Statuses()
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	data, err := json.Marshal(statuses)
	fixturekit.Must(t, err)
	if !strings.Contains(string(data), `"wait":"waiting on owner: owner/repo-a#123 merge"`) {
		t.Fatalf("status API omits the merge wait: %s", data)
	}
	inbox, apiFault := f.s.Inbox(context.Background())
	if apiFault != nil || len(inbox.Entries) != 0 {
		t.Fatalf("edge wait created owner inbox entries: %+v, %v", inbox, apiFault)
	}
	notifications.Pass(context.Background())
	if len(hook.Received()) != 0 {
		t.Fatal("edge wait sent a webhook notification")
	}
}
