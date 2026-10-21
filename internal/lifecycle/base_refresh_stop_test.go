package lifecycle_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stopFixture opens a fresh trace repository with a controllable clock and a
// fake pull request host counting its own calls, for testing when
// base-refresh keeps scheduling or stops (spec#1 to spec#4, spec#11 to
// spec#13).
func stopFixture(t *testing.T) (*config.Config, *trace.Repository, *fakePulls, *baseRefresher, *demoClock) {
	t.Helper()
	cfg, repo, host, clock := fixturekit.BaseRefreshTrace(t)
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Options: service.Options{PullRequests: host, Reconciliation: reconcile.Options{Now: clock.Now}}}}
	return cfg, repo, host, &baseRefresher{s: s, repository: repo}, clock
}
func baseRefreshOperationCount(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	return fixturekit.OperationCount(t, repo, stream, baseRefreshAction)
}

func recordIntegration(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, at time.Time, commit string) {
	t.Helper()
	fixturekit.RecordBaseIntegration(t, repo, stream, at, commit, foremanActor)
}

// TestDependentDeliveryMaintenanceCompletesAfterARestartFollowingIntegration
// covers spec#9 and spec#10: after integration is recorded, the pending
// rebase onto upstream and the dependent's pull-request maintenance
// (retargeting its open pull request) still complete when a restart happens
// between recording the integration and finishing them.
func TestDependentDeliveryMaintenanceCompletesAfterARestartFollowingIntegration(t *testing.T) {
	t.Parallel()
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
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Ready upstream", "findings": []any{}})
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
	t.Parallel()
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
