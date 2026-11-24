package lifecycle_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDependentDeliveryMaintenanceReviewsApprovesAndReconcilesUpstream(t *testing.T) {
	t.Parallel()
	p := journey.MaintenanceFixture(t)
	ctx := context.Background()
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
		t.Fatal("maintenance reopened implementation")
	}
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	ops, err := p.Repository.Operations(p.Stream)
	fixturekit.Must(t, err)
	for _, o := range ops {
		if o.Operation.Action == lifecycle.PublishUpstreamAction {
			t.Fatal("published without renewed approval")
		}
	}
	presented, apiFault := p.S.DeliveryPresentation(ctx, string(p.Stream))
	if apiFault != nil || !presented.Maintenance || !presented.Delivered || presented.Approval != nil {
		t.Fatalf("maintenance presentation %+v %v", presented, apiFault)
	}
	entry, found, err := journey.InspectForTest(p.S).DeliveryEntry(ctx, p.Repository, p.Stream)
	fixturekit.Must(t, err)
	if !found || entry.Kind != api.InboxDelivery {
		t.Fatalf("missing maintenance approval: %+v", entry)
	}
	p.Approve(t, nil)
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	publish := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.PublishUpstreamAction)
	host := p.S.Options.PullRequests.(fixturekit.RepositoryPulls)[p.S.Cfg.Project.Upstream]
	host.Lose = true
	if _, err := p.Publisher().Apply(ctx, publish); err == nil {
		t.Fatal("expected lost hosting response")
	}
	p.Reopen(t)
	result, err = p.Publisher().Apply(ctx, publish)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" || host.Creates != 1 || p.Feature(t) != intake.DeliveredState {
		t.Fatalf("publication %+v creates %d", result, host.Creates)
	}
	records, err := delivery.Publications(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	last := records[len(records)-1]
	if last.Upstream != p.S.Cfg.Project.Upstream || last.PriorURL != p.Pulls.Prs[0].URL {
		t.Fatalf("relationship %+v", last)
	}
	followups, err := followup.Read(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	if len(followups) != 0 {
		t.Fatal("maintenance created implementation units")
	}
	before := len(records)
	_, err = p.Publisher().Apply(ctx, publish)
	fixturekit.Must(t, err)
	records, err = delivery.Publications(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	if len(records) != before || host.Creates != 1 {
		t.Fatal("retry duplicated delivery")
	}
}

func TestBaseRefreshRetryDoesNotDuplicateObservationAndPassStopsScheduling(t *testing.T) {
	t.Parallel()
	p := journey.MaintenanceFixture(t)
	ctx := context.Background()
	refresh := &journey.BaseRefresher{S: p.S, Repository: p.Repository}
	op := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.BaseRefreshAction)
	p.Reopen(t)
	refresh.Repository = p.Repository
	observed, err := refresh.Inspect(ctx, op)
	fixturekit.Must(t, err)
	if observed.State != coreadapter.EffectCompleted {
		t.Fatal(observed)
	}
	_, err = refresh.Apply(ctx, op)
	fixturekit.Must(t, err)
	_, rev, err := lifecycle.BaseObservationAt(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	if rev != 1 {
		t.Fatalf("retry revision %d", rev)
	}
	fixturekit.Jump(p.Clock, p.S.Now().Add(11*time.Minute))
	fixturekit.Must(t, refresh.Pass(ctx))
	next := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.BaseRefreshAction)
	if next.ID != op.ID {
		t.Fatal("base-refresh was scheduled again after integration was recorded")
	}
	_, rev, err = lifecycle.BaseObservationAt(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	if rev != 1 {
		t.Fatalf("unchanged observation revision %d", rev)
	}
}
func TestDependentDeliveryMaintenanceGapKeepsImplementationTerminal(t *testing.T) {
	t.Parallel()
	p := journey.MaintenanceFixture(t)
	ctx := context.Background()
	reviewer := &journey.FinalReviewer{S: p.S, Repository: p.Repository}
	fixturekit.Must(t, reviewer.Pass(ctx))
	op := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.DeliveryReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	fixturekit.Must(t, err)
	p.FinalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Integration exposes a gap", "findings": []any{map[string]any{"severity": "blocking", "criteria": []any{"spec#1"}, "finding": "resume behavior is missing"}}})
		return err
	})
	_, err = reviewer.Apply(ctx, op)
	fixturekit.Must(t, err)
	report, reason, err := reviewer.FinalGate(ctx, p.Stream)
	fixturekit.Must(t, err)
	if !report.Maintenance || reason != "" || len(delivery.ReportBlocking(report)) != 1 || p.Feature(t) != intake.DeliveredState {
		t.Fatalf("maintenance gap: %+v reason %q state %s", report, reason, p.Feature(t))
	}
	units, err := followup.Read(p.Repository, p.Stream)
	fixturekit.Must(t, err)
	if len(units) != 0 {
		t.Fatal("maintenance gap reopened implementation")
	}
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	ops, err := p.Repository.Operations(p.Stream)
	fixturekit.Must(t, err)
	for _, op := range ops {
		if op.Operation.Action == lifecycle.PublishUpstreamAction {
			t.Fatal("maintenance gap allowed publication")
		}
	}
}
func TestBaseIntegrationReleasesUnsealedDependentAfterParentBranchRemoval(t *testing.T) {
	t.Parallel()
	p := journey.MaintenanceFixture(t)
	ctx := context.Background()
	parent := config.WorkstreamID("w_fffffffffffffffffffffffffffffff0")
	child := config.WorkstreamID("w_fffffffffffffffffffffffffffffff1")
	repo := p.Repository
	fixturekit.Must(t, repo.CreateWorkstream(ctx, child, p.S.Now(), intake.OwnerActor))
	_, err := repo.SetWorkstreamBase(ctx, child, parent, 0, p.S.Now())
	fixturekit.Must(t, err)
	sealed, _, _, err := seal.Latest(repo, parent)
	fixturekit.Must(t, err)
	publication := api.DeliveryPublication{Status: delivery.PublicationOpened, Base: "main", Upstream: p.S.Cfg.Project.Upstream, Fork: p.S.Cfg.Project.Fork, Branch: sealed.Branch, Commit: sealed.Base.Commit, PullRequest: 7}
	data, err := json.Marshal(publication)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: 1, ID: delivery.PublicationDocument, Revision: 1, Project: repo.Project(), Workstream: parent, At: p.S.Now(), Actor: execution.ForemanActor, Cause: "delivery"}
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: delivery.PublicationPath, Content: string(data)}}))
	host := p.S.Options.PullRequests.(fixturekit.RepositoryPulls)[p.S.Cfg.Project.Upstream]
	host.Prs = []pulls.PullRequest{{Number: 7, Merged: true, Base: "main", Head: sealed.Branch, HeadRepository: p.S.Cfg.Project.Fork, HeadCommit: sealed.Base.Commit}}
	fixturekit.DemoGit(t, filepath.Dir(p.Clone), "-C", p.Clone, "branch", "-D", sealed.Branch)
	ratified, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, nil))
	fixturekit.Must(t, err)
	h.Workstream, h.ID = child, shed.RatificationDocumentID(1)
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: shed.RatificationPath(1), Content: string(ratified)}}))
	fixturekit.Must(t, p.S.Lifecycle().BaseWaitPass(ctx, repo))
	waiting, err := lifecycle.BaseWaiting(repo, child)
	fixturekit.Must(t, err)
	if !waiting {
		t.Fatal("missing branch did not park the dependent")
	}
	refresh := &journey.BaseRefresher{S: p.S, Repository: repo}
	fixturekit.Must(t, refresh.Pass(ctx))
	op := fixturekit.ActionOperation(t, repo, child, lifecycle.BaseRefreshAction)
	result := journey.SettleOperation(t, p.S, repo, child, op, refresh)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	fixturekit.Must(t, p.S.Lifecycle().BaseWaitPass(ctx, repo))
	waiting, err = lifecycle.BaseWaiting(repo, child)
	fixturekit.Must(t, err)
	if waiting {
		t.Fatal("integrated parent left sealing parked")
	}
	observed, _, err := lifecycle.BaseObservationAt(repo, child)
	fixturekit.Must(t, err)
	if observed.Unavailable || observed.Base.Workstream != "" || observed.Base.Commit == "" {
		t.Fatalf("integration observation %+v", observed)
	}
}
