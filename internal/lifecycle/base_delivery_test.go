package lifecycle_test

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/execution"
	"path/filepath"
	"testing"
	"time"

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
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func maintenanceFixture(t *testing.T) *publicationFixture {
	t.Helper()
	p := newUnpushedPublicationFixture(t, "commit-per-unit", "")
	intakefixture.PrepareDeliveryMaintenance(t, intakefixture.DeliveryMaintenance{
		Config: p.s.cfg, Repository: p.repository, Stream: p.stream, Clone: p.clone, Now: p.s.now,
		Approve: func() { p.publishDraft(t); p.approve(t, nil) },
		Publish: func() (coreadapter.OperationResult, error) {
			return p.publisher().Apply(context.Background(), p.request(t))
		},
		AdvanceUpstream: func(files map[string]string) { advanceUpstream(t, p.shedFixture, files) },
		ForkTip:         func() string { tip, _ := p.forkBranch(t); return tip },
		Refresh: func(host *fixturekit.FakePulls, upstream string) coreadapter.OperationResult {
			p.s.options.PullRequests = fixturekit.RepositoryPulls{upstream: host, p.s.cfg.Project.Fork: p.pulls}
			refresh := &baseRefresher{s: p.s, repository: p.repository}
			fixturekit.Must(t, refresh.Pass(context.Background()))
			op := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.BaseRefreshAction)
			return settleOperation(t, p.s, p.repository, p.stream, op, refresh)
		},
		Observation: func(r *trace.Repository, w config.WorkstreamID) (seal.Base, bool, int, error) {
			o, rev, err := lifecycle.BaseObservationAt(r, w)
			return o.Base, o.Unavailable, rev, err
		},
	})
	return p
}

func TestDependentDeliveryMaintenanceReviewsApprovesAndReconcilesUpstream(t *testing.T) {
	t.Parallel()
	p := maintenanceFixture(t)
	ctx := context.Background()
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
		t.Fatal("maintenance reopened implementation")
	}
	fixturekit.Must(t, p.publisher().Pass(ctx))
	ops, err := p.repository.Operations(p.stream)
	fixturekit.Must(t, err)
	for _, o := range ops {
		if o.Operation.Action == lifecycle.PublishUpstreamAction {
			t.Fatal("published without renewed approval")
		}
	}
	presented, apiFault := p.s.deliveryPresentation(ctx, string(p.stream))
	if apiFault != nil || !presented.Maintenance || !presented.Delivered || presented.Approval != nil {
		t.Fatalf("maintenance presentation %+v %v", presented, apiFault)
	}
	entry, found, err := p.s.deliveryEntry(ctx, p.repository, p.stream)
	fixturekit.Must(t, err)
	if !found || entry.Kind != api.InboxDelivery {
		t.Fatalf("missing maintenance approval: %+v", entry)
	}
	p.approve(t, nil)
	fixturekit.Must(t, p.publisher().Pass(ctx))
	publish := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.PublishUpstreamAction)
	host := p.s.options.PullRequests.(fixturekit.RepositoryPulls)[p.s.cfg.Project.Upstream]
	host.Lose = true
	if _, err := p.publisher().Apply(ctx, publish); err == nil {
		t.Fatal("expected lost hosting response")
	}
	p.reopen(t)
	result, err = p.publisher().Apply(ctx, publish)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" || host.Creates != 1 || p.feature(t) != intake.DeliveredState {
		t.Fatalf("publication %+v creates %d", result, host.Creates)
	}
	records, err := delivery.Publications(p.repository, p.stream)
	fixturekit.Must(t, err)
	last := records[len(records)-1]
	if last.Upstream != p.s.cfg.Project.Upstream || last.PriorURL != p.pulls.Prs[0].URL {
		t.Fatalf("relationship %+v", last)
	}
	followups, err := followup.Read(p.repository, p.stream)
	fixturekit.Must(t, err)
	if len(followups) != 0 {
		t.Fatal("maintenance created implementation units")
	}
	before := len(records)
	_, err = p.publisher().Apply(ctx, publish)
	fixturekit.Must(t, err)
	records, err = delivery.Publications(p.repository, p.stream)
	fixturekit.Must(t, err)
	if len(records) != before || host.Creates != 1 {
		t.Fatal("retry duplicated delivery")
	}
}

func TestBaseRefreshRetryDoesNotDuplicateObservationAndPassStopsScheduling(t *testing.T) {
	t.Parallel()
	p := maintenanceFixture(t)
	ctx := context.Background()
	refresh := &baseRefresher{s: p.s, repository: p.repository}
	op := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.BaseRefreshAction)
	p.reopen(t)
	refresh.repository = p.repository
	observed, err := refresh.Inspect(ctx, op)
	fixturekit.Must(t, err)
	if observed.State != coreadapter.EffectCompleted {
		t.Fatal(observed)
	}
	_, err = refresh.Apply(ctx, op)
	fixturekit.Must(t, err)
	_, rev, err := lifecycle.BaseObservationAt(p.repository, p.stream)
	fixturekit.Must(t, err)
	if rev != 1 {
		t.Fatalf("retry revision %d", rev)
	}
	fixturekit.Jump(p.clock, p.s.now().Add(11*time.Minute))
	fixturekit.Must(t, refresh.Pass(ctx))
	next := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.BaseRefreshAction)
	if next.ID != op.ID {
		t.Fatal("base-refresh was scheduled again after integration was recorded")
	}
	_, rev, err = lifecycle.BaseObservationAt(p.repository, p.stream)
	fixturekit.Must(t, err)
	if rev != 1 {
		t.Fatalf("unchanged observation revision %d", rev)
	}
}
func TestDependentDeliveryMaintenanceGapKeepsImplementationTerminal(t *testing.T) {
	t.Parallel()
	p := maintenanceFixture(t)
	ctx := context.Background()
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	fixturekit.Must(t, reviewer.Pass(ctx))
	op := fixturekit.ActionOperation(t, p.repository, p.stream, lifecycle.DeliveryReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	fixturekit.Must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Integration exposes a gap", "findings": []any{map[string]any{"severity": "blocking", "criteria": []any{"spec#1"}, "finding": "resume behavior is missing"}}})
		return err
	})
	_, err = reviewer.Apply(ctx, op)
	fixturekit.Must(t, err)
	report, reason, err := reviewer.finalGate(ctx, p.stream)
	fixturekit.Must(t, err)
	if !report.Maintenance || reason != "" || len(delivery.ReportBlocking(report)) != 1 || p.feature(t) != intake.DeliveredState {
		t.Fatalf("maintenance gap: %+v reason %q state %s", report, reason, p.feature(t))
	}
	units, err := followup.Read(p.repository, p.stream)
	fixturekit.Must(t, err)
	if len(units) != 0 {
		t.Fatal("maintenance gap reopened implementation")
	}
	fixturekit.Must(t, p.publisher().Pass(ctx))
	ops, err := p.repository.Operations(p.stream)
	fixturekit.Must(t, err)
	for _, op := range ops {
		if op.Operation.Action == lifecycle.PublishUpstreamAction {
			t.Fatal("maintenance gap allowed publication")
		}
	}
}
func TestBaseIntegrationReleasesUnsealedDependentAfterParentBranchRemoval(t *testing.T) {
	t.Parallel()
	p := maintenanceFixture(t)
	ctx := context.Background()
	parent := config.WorkstreamID("w_fffffffffffffffffffffffffffffff0")
	child := config.WorkstreamID("w_fffffffffffffffffffffffffffffff1")
	repo := p.repository
	fixturekit.Must(t, repo.CreateWorkstream(ctx, child, p.s.now(), intake.OwnerActor))
	_, err := repo.SetWorkstreamBase(ctx, child, parent, 0, p.s.now())
	fixturekit.Must(t, err)
	sealed, _, _, err := seal.Latest(repo, parent)
	fixturekit.Must(t, err)
	publication := api.DeliveryPublication{Status: delivery.PublicationOpened, Base: "main", Upstream: p.s.cfg.Project.Upstream, Fork: p.s.cfg.Project.Fork, Branch: sealed.Branch, Commit: sealed.Base.Commit, PullRequest: 7}
	data, err := json.Marshal(publication)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: 1, ID: delivery.PublicationDocument, Revision: 1, Project: repo.Project(), Workstream: parent, At: p.s.now(), Actor: execution.ForemanActor, Cause: "delivery"}
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: delivery.PublicationPath, Content: string(data)}}))
	host := p.s.options.PullRequests.(fixturekit.RepositoryPulls)[p.s.cfg.Project.Upstream]
	host.Prs = []pulls.PullRequest{{Number: 7, Merged: true, Base: "main", Head: sealed.Branch, HeadRepository: p.s.cfg.Project.Fork, HeadCommit: sealed.Base.Commit}}
	fixturekit.DemoGit(t, filepath.Dir(p.clone), "-C", p.clone, "branch", "-D", sealed.Branch)
	ratified, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, nil))
	fixturekit.Must(t, err)
	h.Workstream, h.ID = child, shed.RatificationDocumentID(1)
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: shed.RatificationPath(1), Content: string(ratified)}}))
	fixturekit.Must(t, p.s.lifecycle().BaseWaitPass(ctx, repo))
	waiting, err := lifecycle.BaseWaiting(repo, child)
	fixturekit.Must(t, err)
	if !waiting {
		t.Fatal("missing branch did not park the dependent")
	}
	refresh := &baseRefresher{s: p.s, repository: repo}
	fixturekit.Must(t, refresh.Pass(ctx))
	op := fixturekit.ActionOperation(t, repo, child, lifecycle.BaseRefreshAction)
	result := settleOperation(t, p.s, repo, child, op, refresh)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	fixturekit.Must(t, p.s.lifecycle().BaseWaitPass(ctx, repo))
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
