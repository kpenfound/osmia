package lifecycle_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

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

func stopFixture(t *testing.T) (*config.Config, *trace.Repository, *fixturekit.FakePulls, *journey.BaseRefresher, *fixturekit.DemoClock) {
	t.Helper()
	cfg, repo, host, clock := fixturekit.BaseRefreshTrace(t)
	s := &journey.Service{Cfg: cfg, Projects: []*journey.ActiveProject{journey.RuntimeFor(repo)}, Options: journey.Options{Options: service.Options{PullRequests: host, Reconciliation: reconcile.Options{Now: clock.Now}}}}
	return cfg, repo, host, &journey.BaseRefresher{S: s, Repository: repo}, clock
}
func TestDependentDeliveryMaintenanceCompletesAfterARestartFollowingIntegration(t *testing.T) {
	t.Parallel()
	p := journey.MaintenanceFixture(t)
	ctx := context.Background()
	p.Reopen(t)
	reviewer := &journey.FinalReviewer{S: p.S, Repository: p.Repository}
	fixturekit.Must(t, reviewer.Pass(ctx))
	op := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.DeliveryReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	fixturekit.Must(t, err)
	p.FinalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Ready upstream", "findings": []any{}})
		return err
	})
	result, err := reviewer.Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	if p.Feature(t) != intake.DeliveredState {
		t.Fatal("maintenance did not complete after the restart")
	}
	p.Approve(t, nil)
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	publish := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.PublishUpstreamAction)
	result, err = p.Publisher().Apply(ctx, publish)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatalf("pull-request retargeting after the restart: %+v", result)
	}
	records, err := delivery.Publications(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	last := records[len(records)-1]
	if last.Upstream != p.S.Cfg.Project.Upstream {
		t.Fatalf("the dependent's pull request was not retargeted onto upstream after a restart: %+v", last)
	}
}

func TestDriftRebaseKeepsSchedulingOnceBaseRefreshStopsForAnIntegratedDependency(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	f.Upstream(t)
	f.Stop(t)
	ctx := context.Background()
	repo, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	base, _ := journey.SealDependent(t, f, repo, config.WorkspacesGit, "", map[string]string{"base.go": "base\n"})
	dependent, _ := journey.SealDependent(t, f, repo, config.WorkspacesGit, base, map[string]string{"dependent.go": "dependent\n"})

	h := trace.Header{Schema: "osmia.trace.transition", Version: 1, ID: "building", Revision: 1, Project: repo.Project(), Workstream: dependent, At: f.S.Now(), Actor: intake.OwnerActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.BuildingState, "Units are recorded")
	fixturekit.Must(t, err)

	fixturekit.RecordBaseIntegration(t, repo, dependent, f.S.Now(), "recorded-upstream-sha", execution.ForemanActor)
	refresh := &journey.BaseRefresher{S: f.S, Repository: repo}
	fixturekit.Must(t, refresh.Pass(ctx))
	if n := fixturekit.OperationCount(t, repo, dependent, lifecycle.BaseRefreshAction); n != 0 {
		t.Fatalf("base-refresh was scheduled after integration was recorded: %d operations", n)
	}

	fm := cadenceForeman(f, repo, "6h")
	fm.NextDrift = time.Time{}
	fixturekit.Must(t, fm.Drifts(ctx))
	checkDrifts(t, repo, dependent, 1, "once integration pinned the base observation onto upstream")
	if reason := intakefixture.TransitionByID(t, repo, dependent, "drift-1").Reason; !strings.HasPrefix(reason, "the base workstream branch moved") {
		t.Fatalf("drift rebase reason %q", reason)
	}
}
