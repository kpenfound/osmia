package lifecycle_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func stopFixture(t *testing.T) (*config.Config, *trace.Repository, *fixturekit.FakePulls, *baseRefresher, *fixturekit.DemoClock) {
	t.Helper()
	cfg, repo, host, clock := fixturekit.BaseRefreshTrace(t)
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Options: service.Options{PullRequests: host, Reconciliation: reconcile.Options{Now: clock.Now}}}}
	return cfg, repo, host, &baseRefresher{s: s, repository: repo}, clock
}
func baseRefreshOperationCount(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) int {
	t.Helper()
	return fixturekit.OperationCount(t, repo, stream, lifecycle.BaseRefreshAction)
}

func recordIntegration(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, at time.Time, commit string) {
	t.Helper()
	fixturekit.RecordBaseIntegration(t, repo, stream, at, commit, service.TestForemanActor)
}

func TestDependentDeliveryMaintenanceCompletesAfterARestartFollowingIntegration(t *testing.T) {
	t.Parallel()
	p := maintenanceFixture(t)
	ctx := context.Background()
	p.reopen(t)
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	fixturekit.Must(t, reviewer.Pass(ctx))
	op := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.DeliveryReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	fixturekit.Must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Ready upstream", "findings": []any{}})
		return err
	})
	result, err := reviewer.Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	if p.feature(t) != intake.DeliveredState {
		t.Fatal("maintenance did not complete after the restart")
	}
	p.approve(t, nil)
	fixturekit.Must(t, p.publisher().Pass(ctx))
	publish := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.PublishUpstreamAction)
	result, err = p.publisher().Apply(ctx, publish)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("pull-request retargeting after the restart: %+v", result)
	}
	records, err := delivery.Publications(p.repository, p.stream)
	fixturekit.Must(t, err)
	last := records[len(records)-1]
	if last.Upstream != p.s.cfg.Project.Upstream {
		t.Fatalf("the dependent's pull request was not retargeted onto upstream after a restart: %+v", last)
	}
}

func TestDriftRebaseKeepsSchedulingOnceBaseRefreshStopsForAnIntegratedDependency(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	f.upstream(t)
	f.stop(t)
	ctx := context.Background()
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	base, _ := sealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
	dependent, _ := sealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})

	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "building", Revision: 1, Project: repo.Project(), Workstream: dependent, At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.BuildingState, "Units are recorded")
	fixturekit.Must(t, err)

	recordIntegration(t, repo, dependent, f.s.now(), "recorded-upstream-sha")
	refresh := &baseRefresher{s: f.s, repository: repo}
	fixturekit.Must(t, refresh.Pass(ctx))
	if n := baseRefreshOperationCount(t, repo, dependent); n != 0 {
		t.Fatalf("base-refresh was scheduled after integration was recorded: %d operations", n)
	}

	fm := cadenceForeman(f, repo, "6h")
	fm.nextDrift = time.Time{}
	fixturekit.Must(t, fm.drifts(ctx))
	checkDrifts(t, repo, dependent, 1, "once integration pinned the base observation onto upstream")
	if reason := intakefixture.TransitionByID(t, repo, dependent, "drift-1").Reason; !strings.HasPrefix(reason, "the base workstream branch moved") {
		t.Fatalf("drift rebase reason %q", reason)
	}
}
