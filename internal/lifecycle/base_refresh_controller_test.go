package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func stopFixture(t *testing.T) (*config.Config, *trace.Repository, *fixturekit.FakePulls, *BaseRefresher, *fixturekit.DemoClock) {
	t.Helper()
	cfg, repo, host, clock := fixturekit.BaseRefreshTrace(t)
	c := New(Dependencies{Options: Execution{PullRequests: host}, Step: func(string) error { return nil }, Now: clock.Now, About: func(*trace.Repository) *config.Config { return cfg }, Current: func() *config.Config { return cfg }, Store: func() *runtime.Store { return nil }, Polls: &PullRequestAttempts{}})
	return cfg, repo, host, &BaseRefresher{S: c, Repository: repo}, clock
}
func sealStream(t *testing.T, repo *trace.Repository) {
	t.Helper()
	fixturekit.SealDependentStream(t, repo, stream, quiet, featureBranch(stream), foremanActor)
}
func deliverWithPullRequest(t *testing.T, repo *trace.Repository, cfg *config.Config, pr int) DeliveryPublication {
	t.Helper()
	return fixturekit.RecordDeliveredPublication(t, repo, cfg, stream, featureBranch(stream), pr, foremanActor)
}

var actionOperation = fixturekit.ActionOperation
var jump = fixturekit.Jump

func TestBaseRefreshPassKeepsSchedulingUndeliveredWorkstreamWithUnresolvedDependency(t *testing.T) {
	t.Parallel()
	_, repo, host, refresh, clock := stopFixture(t)
	ctx := context.Background()
	sealStream(t, repo)
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, stream, BaseRefreshAction)
	result, err := refresh.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 0 {
		t.Fatalf("an undelivered workstream made %d own-PR lookups", host.Finds)
	}
	jump(clock, refresh.S.Now().Add(11*time.Minute))
	must(t, refresh.Pass(ctx))
	next := actionOperation(t, repo, stream, BaseRefreshAction)
	if next.ID == op.ID {
		t.Fatal("base-refresh was not scheduled again for the unresolved dependency")
	}
}

func TestBaseRefreshPassKeepsSchedulingDeliveredWithoutOwnPullRequestAndUnresolvedDependency(t *testing.T) {
	t.Parallel()
	_, repo, host, refresh, clock := stopFixture(t)
	ctx := context.Background()
	sealStream(t, repo)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: DeliveredState, Revision: 1, Project: project, Workstream: stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "fixture"}
	_, err := repo.SetFeatureState(ctx, h, DeliveredState, "delivered by merging outside the factory")
	must(t, err)
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, stream, BaseRefreshAction)
	result, err := refresh.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 0 {
		t.Fatalf("a workstream delivered without a pull request made %d own-PR lookups", host.Finds)
	}
	jump(clock, refresh.S.Now().Add(11*time.Minute))
	must(t, refresh.Pass(ctx))
	next := actionOperation(t, repo, stream, BaseRefreshAction)
	if next.ID == op.ID {
		t.Fatal("base-refresh was not scheduled again for the delivered workstream without its own pull request")
	}
	result, err = refresh.Apply(ctx, next)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 0 {
		t.Fatalf("a later refresh made an own-PR lookup for a workstream without a pull request: %d", host.Finds)
	}
}

func TestBaseRefreshPassKeepsSchedulingDeliveredWithOpenOwnPullRequestAndUnresolvedDependency(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh, clock := stopFixture(t)
	ctx := context.Background()
	sealStream(t, repo)
	publication := deliverWithPullRequest(t, repo, cfg, 42)
	host.Prs = []pulls.PullRequest{{Number: 42, State: "open", Head: publication.Branch, HeadRepository: publication.Fork}}
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, stream, BaseRefreshAction)
	result, err := refresh.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.Finds)
	}
	if finished, err := ownPullRequestFinished(repo, stream); err != nil || finished {
		t.Fatalf("an open own pull request reported finished: %v err %v", finished, err)
	}
	jump(clock, refresh.S.Now().Add(11*time.Minute))
	must(t, refresh.Pass(ctx))
	next := actionOperation(t, repo, stream, BaseRefreshAction)
	if next.ID == op.ID {
		t.Fatal("base-refresh was not scheduled again for the delivered workstream with an open own pull request")
	}
	result, err = refresh.Apply(ctx, next)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 2 {
		t.Fatalf("a later refresh did not look up the open own pull request again: %d", host.Finds)
	}
}

func TestBaseRefreshPassStopsAndResolveBaseMakesNoRemoteCallOnceIntegrationIsRecorded(t *testing.T) {
	t.Parallel()
	cfg, repo, host, refresh, _ := stopFixture(t)
	ctx := context.Background()
	sealStream(t, repo)
	before, err := repo.WorkstreamBase(stream)
	must(t, err)
	fixturekit.RecordBaseIntegration(t, repo, stream, fixturekit.DemoStart, "recorded-upstream-sha", foremanActor)

	for i := 0; i < 3; i++ {
		must(t, refresh.Pass(ctx))
		if n := fixturekit.OperationCount(t, repo, stream, BaseRefreshAction); n != 0 {
			t.Fatalf("pass %d scheduled a base-refresh after integration was recorded: %d operations", i, n)
		}
	}
	if host.Finds != 0 {
		t.Fatalf("Pass made %d pull-request lookups", host.Finds)
	}

	repo.Close()
	reopened, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	refresh.Repository = reopened
	for i := 0; i < 2; i++ {
		must(t, refresh.Pass(ctx))
		if n := fixturekit.OperationCount(t, reopened, stream, BaseRefreshAction); n != 0 {
			t.Fatalf("pass %d after a restart scheduled a base-refresh: %d operations", i, n)
		}
	}

	after, err := reopened.WorkstreamBase(stream)
	must(t, err)
	if after != before {
		t.Fatalf("the declared dependency changed after refreshes stopped: before %+v after %+v", before, after)
	}

	g := &fixturekit.BaseProvider{Upstream: "upstream-sha", Tip: "base-sha"}
	selected, err := refresh.S.ResolveBase(ctx, cfg, reopened, stream, g)
	must(t, err)
	if selected.Workstream != "" || selected.Commit != "recorded-upstream-sha" {
		t.Fatalf("resolveBase did not return the recorded upstream base: %+v", selected)
	}
	if g.Remotes != 0 || g.Fetches != 0 {
		t.Fatalf("resolveBase fetched upstream after integration was recorded: remotes=%d fetches=%d", g.Remotes, g.Fetches)
	}
	if host.Finds != 0 {
		t.Fatalf("resolveBase looked up a pull request after integration was recorded: %d", host.Finds)
	}
}

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
	host.Prs = []pulls.PullRequest{pr}
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, stream, BaseRefreshAction)
	result, err := refresh.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("base-refresh apply: %+v", result)
	}
	if host.Finds != 1 {
		t.Fatalf("own-PR lookup calls: %d", host.Finds)
	}
	finished, err := ownPullRequestFinished(repo, stream)
	must(t, err)
	if !finished {
		t.Fatal("the recorded outcome did not mark the workstream finished")
	}

	for i := 0; i < 3; i++ {
		must(t, refresh.Pass(ctx))
		if n := fixturekit.OperationCount(t, repo, stream, BaseRefreshAction); n != 1 {
			t.Fatalf("pass %d scheduled a base-refresh for a finished workstream: %d operations", i, n)
		}
	}
	if host.Finds != 1 {
		t.Fatalf("a later pass made an own-PR lookup for a finished workstream: %d", host.Finds)
	}

	repo.Close()
	reopened, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	refresh.Repository = reopened
	for i := 0; i < 2; i++ {
		must(t, refresh.Pass(ctx))
		if n := fixturekit.OperationCount(t, reopened, stream, BaseRefreshAction); n != 1 {
			t.Fatalf("pass %d after a restart scheduled a base-refresh: %d operations", i, n)
		}
	}
	if host.Finds != 1 {
		t.Fatalf("a pass after a restart made an own-PR lookup: %d", host.Finds)
	}

	after, err := reopened.WorkstreamBase(stream)
	must(t, err)
	if after != before {
		t.Fatalf("the declared dependency changed after refreshes stopped: before %+v after %+v", before, after)
	}
	if _, revision, err := BaseObservationAt(reopened, stream); err != nil || revision != 0 {
		t.Fatalf("the dependency was reported integrated: revision %d err %v", revision, err)
	}
}
func TestBaseRefreshPassStopsAfterOwnPullRequestRecordedMerged(t *testing.T) {
	t.Parallel()
	testBaseRefreshPassStopsAfterOwnPullRequestOutcome(t, true)
}
func TestBaseRefreshPassStopsAfterOwnPullRequestRecordedClosed(t *testing.T) {
	t.Parallel()
	testBaseRefreshPassStopsAfterOwnPullRequestOutcome(t, false)
}

func TestBaseRefreshPassStopsForAbandonedWorkstreamRegardlessOfParentIntegration(t *testing.T) {
	t.Parallel()
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
				fixturekit.RecordBaseIntegration(t, repo, stream, fixturekit.DemoStart, "recorded-upstream-sha", foremanActor)
			}
			h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "owner-abandoned", Revision: 1, Project: project, Workstream: stream, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "fixture"}
			_, err := repo.SetFeatureState(ctx, h, AbandonedState, "no longer needed")
			must(t, err)
			for i := 0; i < 3; i++ {
				must(t, refresh.Pass(ctx))
				if n := fixturekit.OperationCount(t, repo, stream, BaseRefreshAction); n != 0 {
					t.Fatalf("pass %d scheduled a base-refresh for an abandoned workstream: %d operations", i, n)
				}
			}
			if host.Finds != 0 {
				t.Fatalf("Pass made %d pull-request lookups for an abandoned workstream", host.Finds)
			}
		})
	}
}
