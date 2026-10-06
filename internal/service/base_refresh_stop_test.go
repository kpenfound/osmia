package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stopFixture opens a fresh trace repository with a controllable clock and a
// fake pull request host counting its own calls, for testing when
// base-refresh keeps scheduling or stops (spec#1 to spec#4, spec#11 to
// spec#13).
func stopFixture(t *testing.T) (*config.Config, *trace.Repository, *fakePulls, *baseRefresher, *demoClock) {
	t.Helper()
	_, cfg := conversationFixture(t, "base-stop-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	host := &fakePulls{}
	clock := &demoClock{now: demoStart}
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{PullRequests: host, Reconciliation: reconcile.Options{Now: clock.Now}}}
	return cfg, repo, host, &baseRefresher{s: s, repository: repo}, clock
}

// baseRefreshOperationCount counts stream's recorded base-refresh operations.
func baseRefreshOperationCount(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	ops, err := repo.Operations(stream)
	must(t, err)
	n := 0
	for _, o := range ops {
		if o.Operation.Action == baseRefreshAction {
			n++
		}
	}
	return n
}

// recordIntegration directly records that stream's dependency has
// integrated into upstream at commit, without running resolveBase or any
// remote call, as a prior base-refresh would have.
func recordIntegration(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, at time.Time, commit string) {
	t.Helper()
	obs := baseObservation{Base: seal.Base{Remote: "origin", Branch: "main", Commit: commit}}
	data, err := json.Marshal(obs)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: baseObservationDocument, Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: foremanActor, Cause: "fixture"}
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: "base-observation.json", Content: string(data)}}))
}

// TestBaseRefreshPassKeepsSchedulingUndeliveredWorkstreamWithUnresolvedDependency
// covers spec#1: an undelivered workstream whose dependency has not
// integrated, and which is not abandoned, keeps getting base-refresh at the
// existing cadence.
func TestBaseRefreshPassKeepsSchedulingUndeliveredWorkstreamWithUnresolvedDependency(t *testing.T) {
	_, repo, host, refresh, clock := stopFixture(t)
	ctx := context.Background()
	sealStream(t, repo)
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	result, err := refresh.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.finds != 0 {
		t.Fatalf("an undelivered workstream made %d own-PR lookups", host.finds)
	}
	// The dependency has not integrated, so once the check's cooldown has
	// elapsed the next Pass asks for another refresh.
	jump(clock, refresh.s.now().Add(2*time.Minute))
	must(t, refresh.Pass(ctx))
	next := actionOperation(t, repo, stream, baseRefreshAction)
	if next.ID == op.ID {
		t.Fatal("base-refresh was not scheduled again for the unresolved dependency")
	}
}

// TestBaseRefreshPassKeepsSchedulingDeliveredWithoutOwnPullRequestAndUnresolvedDependency
// covers spec#11: a workstream delivered without a pull request of its own
// (merged outside the factory) is not finished, so while its dependency has
// not integrated it keeps getting base-refresh, and those refreshes make no
// own-PR lookup.
func TestBaseRefreshPassKeepsSchedulingDeliveredWithoutOwnPullRequestAndUnresolvedDependency(t *testing.T) {
	_, repo, host, refresh, clock := stopFixture(t)
	ctx := context.Background()
	sealStream(t, repo)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: DeliveredState, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "fixture"}
	_, err := repo.SetFeatureState(ctx, h, DeliveredState, "delivered by merging outside the factory")
	must(t, err)
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	result, err := refresh.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.finds != 0 {
		t.Fatalf("a workstream delivered without a pull request made %d own-PR lookups", host.finds)
	}
	jump(clock, refresh.s.now().Add(2*time.Minute))
	must(t, refresh.Pass(ctx))
	next := actionOperation(t, repo, stream, baseRefreshAction)
	if next.ID == op.ID {
		t.Fatal("base-refresh was not scheduled again for the delivered workstream without its own pull request")
	}
	result, err = refresh.Apply(ctx, next)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.finds != 0 {
		t.Fatalf("a later refresh made an own-PR lookup for a workstream without a pull request: %d", host.finds)
	}
}

// TestBaseRefreshPassKeepsSchedulingDeliveredWithOpenOwnPullRequestAndUnresolvedDependency
// covers spec#2: a delivered workstream whose own pull request is open, with
// no outcome recorded yet, is not finished, so while its dependency has not
// integrated and it is not abandoned, it keeps getting base-refresh at the
// existing cadence, with each refresh looking the open pull request up.
func TestBaseRefreshPassKeepsSchedulingDeliveredWithOpenOwnPullRequestAndUnresolvedDependency(t *testing.T) {
	cfg, repo, host, refresh, clock := stopFixture(t)
	ctx := context.Background()
	sealStream(t, repo)
	publication := deliverWithPullRequest(t, repo, cfg, 42)
	host.prs = []pulls.PullRequest{{Number: 42, State: "open", Head: publication.Branch, HeadRepository: publication.Fork}}
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	result, err := refresh.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.finds)
	}
	if finished, err := ownPullRequestFinished(repo, stream); err != nil || finished {
		t.Fatalf("an open own pull request reported finished: %v err %v", finished, err)
	}
	jump(clock, refresh.s.now().Add(2*time.Minute))
	must(t, refresh.Pass(ctx))
	next := actionOperation(t, repo, stream, baseRefreshAction)
	if next.ID == op.ID {
		t.Fatal("base-refresh was not scheduled again for the delivered workstream with an open own pull request")
	}
	result, err = refresh.Apply(ctx, next)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.finds != 2 {
		t.Fatalf("a later refresh did not look up the open own pull request again: %d", host.finds)
	}
}

// TestBaseRefreshPassStopsAndResolveBaseMakesNoRemoteCallOnceIntegrationIsRecorded
// covers spec#3, spec#7, spec#8 and spec#12: once a durable observation
// records that the dependency has integrated into upstream, Pass schedules
// no further base-refresh, that holds across a restart, resolveBase returns
// the recorded upstream base with no Git fetch and no pull-request lookup,
// and the declared dependency and its history stay intact.
func TestBaseRefreshPassStopsAndResolveBaseMakesNoRemoteCallOnceIntegrationIsRecorded(t *testing.T) {
	cfg, repo, host, refresh, _ := stopFixture(t)
	ctx := context.Background()
	sealStream(t, repo)
	before, err := repo.WorkstreamBase(stream)
	must(t, err)
	recordIntegration(t, repo, stream, demoStart, "recorded-upstream-sha")

	for i := 0; i < 3; i++ {
		must(t, refresh.Pass(ctx))
		if n := baseRefreshOperationCount(t, repo, stream); n != 0 {
			t.Fatalf("pass %d scheduled a base-refresh after integration was recorded: %d operations", i, n)
		}
	}
	if host.finds != 0 {
		t.Fatalf("Pass made %d pull-request lookups", host.finds)
	}

	// A restart loses nothing: the stopping fact is durable (spec#7).
	repo.Close()
	reopened, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	refresh.repository = reopened
	for i := 0; i < 2; i++ {
		must(t, refresh.Pass(ctx))
		if n := baseRefreshOperationCount(t, reopened, stream); n != 0 {
			t.Fatalf("pass %d after a restart scheduled a base-refresh: %d operations", i, n)
		}
	}

	after, err := reopened.WorkstreamBase(stream)
	must(t, err)
	if after != before {
		t.Fatalf("the declared dependency changed after refreshes stopped: before %+v after %+v", before, after)
	}

	// resolveBase itself checks the recorded integration before any remote
	// operation, so it makes no Git fetch, no parent pull-request lookup
	// and no own pull-request lookup (spec#8).
	g := &baseProvider{upstream: "upstream-sha", tip: "base-sha"}
	selected, err := refresh.s.resolveBase(ctx, cfg, reopened, stream, g)
	must(t, err)
	if selected.Workstream != "" || selected.Commit != "recorded-upstream-sha" {
		t.Fatalf("resolveBase did not return the recorded upstream base: %+v", selected)
	}
	if g.remotes != 0 || g.fetches != 0 {
		t.Fatalf("resolveBase fetched upstream after integration was recorded: remotes=%d fetches=%d", g.remotes, g.fetches)
	}
	if host.finds != 0 {
		t.Fatalf("resolveBase looked up a pull request after integration was recorded: %d", host.finds)
	}
}

// testBaseRefreshPassStopsAfterOwnPullRequestOutcome covers spec#4 and
// spec#14: once a refresh has recorded the workstream's own pull request
// merged or closed, Pass schedules no further base-refresh, even though the
// dependency (quiet) has never integrated, and that holds across a restart.
func testBaseRefreshPassStopsAfterOwnPullRequestOutcome(t *testing.T, merged bool) {
	cfg, repo, host, refresh, _ := stopFixture(t)
	ctx := context.Background()
	sealStream(t, repo)
	before, err := repo.WorkstreamBase(stream)
	must(t, err)
	publication := deliverWithPullRequest(t, repo, cfg, 42)
	pr := pulls.PullRequest{Number: 42, State: "closed", Head: publication.Branch, HeadRepository: publication.Fork}
	if merged {
		pr.Merged = true
	}
	host.prs = []pulls.PullRequest{pr}
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	result, err := refresh.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.finds)
	}
	finished, err := ownPullRequestFinished(repo, stream)
	must(t, err)
	if !finished {
		t.Fatal("the recorded outcome did not mark the workstream finished")
	}

	for i := 0; i < 3; i++ {
		must(t, refresh.Pass(ctx))
		if n := baseRefreshOperationCount(t, repo, stream); n != 1 {
			t.Fatalf("pass %d scheduled a base-refresh for a finished workstream: %d operations", i, n)
		}
	}
	if host.finds != 1 {
		t.Fatalf("a later pass made an own-PR lookup for a finished workstream: %d", host.finds)
	}

	// A restart loses nothing: the recorded outcome is durable (spec#7).
	repo.Close()
	reopened, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	refresh.repository = reopened
	for i := 0; i < 2; i++ {
		must(t, refresh.Pass(ctx))
		if n := baseRefreshOperationCount(t, reopened, stream); n != 1 {
			t.Fatalf("pass %d after a restart scheduled a base-refresh: %d operations", i, n)
		}
	}
	if host.finds != 1 {
		t.Fatalf("a pass after a restart made an own-PR lookup: %d", host.finds)
	}

	// The dependency never integrated, and its declared record is untouched
	// (spec#12).
	after, err := reopened.WorkstreamBase(stream)
	must(t, err)
	if after != before {
		t.Fatalf("the declared dependency changed after refreshes stopped: before %+v after %+v", before, after)
	}
	if _, revision, err := baseObservationAt(reopened, stream); err != nil || revision != 0 {
		t.Fatalf("the dependency was reported integrated: revision %d err %v", revision, err)
	}
}

func TestBaseRefreshPassStopsAfterOwnPullRequestRecordedMerged(t *testing.T) {
	testBaseRefreshPassStopsAfterOwnPullRequestOutcome(t, true)
}

func TestBaseRefreshPassStopsAfterOwnPullRequestRecordedClosed(t *testing.T) {
	testBaseRefreshPassStopsAfterOwnPullRequestOutcome(t, false)
}

// TestBaseRefreshPassStopsForAbandonedWorkstreamRegardlessOfParentIntegration
// covers spec#4 and spec#14: an abandoned workstream gets no base-refresh,
// whether or not its dependency has integrated into upstream.
func TestBaseRefreshPassStopsForAbandonedWorkstreamRegardlessOfParentIntegration(t *testing.T) {
	for _, integrated := range []bool{false, true} {
		name := "parent-unintegrated"
		if integrated {
			name = "parent-integrated"
		}
		t.Run(name, func(t *testing.T) {
			_, repo, host, refresh, _ := stopFixture(t)
			ctx := context.Background()
			sealStream(t, repo)
			if integrated {
				recordIntegration(t, repo, stream, demoStart, "recorded-upstream-sha")
			}
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "owner-abandoned", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "fixture"}
			_, err := repo.SetFeatureState(ctx, h, AbandonedState, "no longer needed")
			must(t, err)
			for i := 0; i < 3; i++ {
				must(t, refresh.Pass(ctx))
				if n := baseRefreshOperationCount(t, repo, stream); n != 0 {
					t.Fatalf("pass %d scheduled a base-refresh for an abandoned workstream: %d operations", i, n)
				}
			}
			if host.finds != 0 {
				t.Fatalf("Pass made %d pull-request lookups for an abandoned workstream", host.finds)
			}
		})
	}
}

// TestDependentDeliveryMaintenanceCompletesAfterARestartFollowingIntegration
// covers spec#9 and spec#10: after integration is recorded, the pending
// rebase onto upstream and the dependent's pull-request maintenance
// (retargeting its open pull request) still complete when a restart happens
// between recording the integration and finishing them.
func TestDependentDeliveryMaintenanceCompletesAfterARestartFollowingIntegration(t *testing.T) {
	p := maintenanceFixture(t)
	ctx := context.Background()
	// Integration was just recorded by maintenanceFixture; restart before
	// the dependent's rebase and pull-request maintenance complete.
	p.reopen(t)
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	must(t, reviewer.Pass(ctx))
	op := actionOperation(t, p.repository, p.stream, deliveryReviewAction)
	in, err := decodeFinalReview(op)
	must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Ready upstream", "criteria": []any{map[string]any{"criterion": "spec#1", "evidence": "resume.go"}, map[string]any{"criterion": "spec#2", "evidence": "dedupe.go"}}})
		return err
	})
	result, err := reviewer.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	if p.feature(t) != DeliveredState {
		t.Fatal("maintenance did not complete after the restart")
	}
	p.approve(t, nil)
	must(t, p.publisher().Pass(ctx))
	publish := actionOperation(t, p.repository, p.stream, publishUpstreamAction)
	result, err = p.publisher().Apply(ctx, publish)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("pull-request retargeting after the restart: %+v", result)
	}
	records, err := publications(p.repository, p.stream)
	must(t, err)
	last := records[len(records)-1]
	if last.Upstream != p.s.cfg.Project.Upstream {
		t.Fatalf("the dependent's pull request was not retargeted onto upstream after a restart: %+v", last)
	}
}

// TestDriftRebaseKeepsSchedulingOnceBaseRefreshStopsForAnIntegratedDependency
// covers spec#13: once a dependent's base-refresh has stopped because its
// dependency's integration into upstream is recorded, the upstream_rebase
// drift schedule is unaffected by that stop. It still asks for a drift
// rebase of the still-building dependent, now that the recorded observation
// points it onto upstream directly rather than its parent's branch.
func TestDriftRebaseKeepsSchedulingOnceBaseRefreshStopsForAnIntegratedDependency(t *testing.T) {
	f := newDebateFixture(t, 1, 1)
	f.upstream(t)
	f.stop(t)
	ctx := context.Background()
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repo.Close()
	base, _ := sealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
	dependent, _ := sealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})

	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "building", Revision: 1, Project: repo.Project(), Workstream: dependent, At: f.s.now(), Actor: ownerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, BuildingState, "Units are recorded")
	must(t, err)

	recordIntegration(t, repo, dependent, f.s.now(), "recorded-upstream-sha")
	refresh := &baseRefresher{s: f.s, repository: repo}
	must(t, refresh.Pass(ctx))
	if n := baseRefreshOperationCount(t, repo, dependent); n != 0 {
		t.Fatalf("base-refresh was scheduled after integration was recorded: %d operations", n)
	}

	fm := cadenceForeman(f, repo, "6h")
	fm.nextDrift = time.Time{}
	must(t, fm.drifts(ctx))
	checkDrifts(t, repo, dependent, 1, "once integration pinned the base observation onto upstream")
	if reason := transitionByID(t, repo, dependent, "drift-1").Reason; !strings.HasPrefix(reason, "the base workstream branch moved") {
		t.Fatalf("drift rebase reason %q", reason)
	}
}
