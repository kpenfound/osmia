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
func baseRefreshOperationCount(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	return fixturekit.OperationCount(t, repo, stream, BaseRefreshAction)
}

func recordIntegration(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, at time.Time, commit string) {
	t.Helper()
	fixturekit.RecordBaseIntegration(t, repo, stream, at, commit, foremanActor)
}

var actionOperation = fixturekit.ActionOperation
var jump = fixturekit.Jump

// TestBaseRefreshPassKeepsSchedulingUndeliveredWorkstreamWithUnresolvedDependency
// covers spec#1: an undelivered workstream whose dependency has not
// integrated, and which is not abandoned, keeps getting base-refresh at the
// existing cadence.
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
	// The dependency has not integrated, so once the check's cooldown has
	// elapsed the next Pass asks for another refresh.
	jump(clock, refresh.S.Now().Add(11*time.Minute))
	must(t, refresh.Pass(ctx))
	next := actionOperation(t, repo, stream, BaseRefreshAction)
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
	t.Parallel()
	_, repo, host, refresh, clock := stopFixture(t)
	ctx := context.Background()
	sealStream(t, repo)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: DeliveredState, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "fixture"}
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

// TestBaseRefreshPassKeepsSchedulingDeliveredWithOpenOwnPullRequestAndUnresolvedDependency
// covers spec#2: a delivered workstream whose own pull request is open, with
// no outcome recorded yet, is not finished, so while its dependency has not
// integrated and it is not abandoned, it keeps getting base-refresh at the
// existing cadence, with each refresh looking the open pull request up.
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

// TestBaseRefreshPassStopsAndResolveBaseMakesNoRemoteCallOnceIntegrationIsRecorded
// covers spec#3, spec#7, spec#8 and spec#12: once a durable observation
// records that the dependency has integrated into upstream, Pass schedules
// no further base-refresh, that holds across a restart, resolveBase returns
// the recorded upstream base with no Git fetch and no pull-request lookup,
// and the declared dependency and its history stay intact.
func TestBaseRefreshPassStopsAndResolveBaseMakesNoRemoteCallOnceIntegrationIsRecorded(t *testing.T) {
	t.Parallel()
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
	if host.Finds != 0 {
		t.Fatalf("Pass made %d pull-request lookups", host.Finds)
	}

	// A restart loses nothing: the stopping fact is durable (spec#7).
	repo.Close()
	reopened, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	refresh.Repository = reopened
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
		if n := baseRefreshOperationCount(t, repo, stream); n != 1 {
			t.Fatalf("pass %d scheduled a base-refresh for a finished workstream: %d operations", i, n)
		}
	}
	if host.Finds != 1 {
		t.Fatalf("a later pass made an own-PR lookup for a finished workstream: %d", host.Finds)
	}

	// A restart loses nothing: the recorded outcome is durable (spec#7).
	repo.Close()
	reopened, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	refresh.Repository = reopened
	for i := 0; i < 2; i++ {
		must(t, refresh.Pass(ctx))
		if n := baseRefreshOperationCount(t, reopened, stream); n != 1 {
			t.Fatalf("pass %d after a restart scheduled a base-refresh: %d operations", i, n)
		}
	}
	if host.Finds != 1 {
		t.Fatalf("a pass after a restart made an own-PR lookup: %d", host.Finds)
	}

	// The dependency never integrated, and its declared record is untouched
	// (spec#12).
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

// TestBaseRefreshPassStopsForAbandonedWorkstreamRegardlessOfParentIntegration
// covers spec#4 and spec#14: an abandoned workstream gets no base-refresh,
// whether or not its dependency has integrated into upstream.
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
			if host.Finds != 0 {
				t.Fatalf("Pass made %d pull-request lookups for an abandoned workstream", host.Finds)
			}
		})
	}
}
