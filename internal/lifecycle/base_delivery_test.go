package lifecycle_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type repositoryPulls = fixturekit.RepositoryPulls

func maintenanceFixture(t *testing.T) *publicationFixture {
	t.Helper()
	p := newPublicationFixture(t, "commit-per-unit")
	intakefixture.PrepareDeliveryMaintenance(t, intakefixture.DeliveryMaintenance{
		Config: p.s.cfg, Repository: p.repository, Stream: p.stream, Clone: p.clone, Now: p.s.now,
		Approve: func() { p.approve(t, nil) },
		Publish: func() (coreadapter.OperationResult, error) {
			return p.publisher().Apply(context.Background(), p.request(t))
		},
		AdvanceUpstream: func(files map[string]string) { advanceUpstream(t, p.shedFixture, files) },
		ForkTip:         func() string { tip, _ := p.forkBranch(t); return tip },
		Refresh: func(host *fakePulls, upstream string) coreadapter.OperationResult {
			p.s.options.PullRequests = repositoryPulls{upstream: host, p.s.cfg.Project.Fork: p.pulls}
			refresh := &baseRefresher{s: p.s, repository: p.repository}
			must(t, refresh.Pass(context.Background()))
			op := actionOperation(t, p.repository, p.stream, baseRefreshAction)
			return settleOperation(t, p.s, p.repository, p.stream, op, refresh)
		},
		Observation: func(r *trace.Repository, w config.WorkstreamID) (seal.Base, bool, int, error) {
			o, rev, err := baseObservationAt(r, w)
			return o.Base, o.Unavailable, rev, err
		},
	})
	return p
}

var actionOperation = fixturekit.ActionOperation

func TestDependentDeliveryMaintenanceReviewsApprovesAndReconcilesUpstream(t *testing.T) {
	t.Parallel()
	p := maintenanceFixture(t)
	ctx := context.Background()
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
		t.Fatal("maintenance reopened implementation")
	}
	must(t, p.publisher().Pass(ctx))
	ops, err := p.repository.Operations(p.stream)
	must(t, err)
	for _, o := range ops {
		if o.Operation.Action == publishUpstreamAction {
			t.Fatal("published without renewed approval")
		}
	}
	presented, api := p.s.deliveryPresentation(ctx, string(p.stream))
	if api != nil || !presented.Maintenance || !presented.Delivered || presented.Approval != nil {
		t.Fatalf("maintenance presentation %+v %v", presented, api)
	}
	entry, found, err := p.s.deliveryEntry(ctx, p.repository, p.stream)
	must(t, err)
	if !found || entry.Kind != InboxDelivery {
		t.Fatalf("missing maintenance approval: %+v", entry)
	}
	p.approve(t, nil)
	must(t, p.publisher().Pass(ctx))
	publish := actionOperation(t, p.repository, p.stream, publishUpstreamAction)
	host := p.s.options.PullRequests.(repositoryPulls)[p.s.cfg.Project.Upstream]
	host.Lose = true
	if _, err := p.publisher().Apply(ctx, publish); err == nil {
		t.Fatal("expected lost hosting response")
	}
	p.reopen(t)
	result, err = p.publisher().Apply(ctx, publish)
	must(t, err)
	if result.Outcome != "succeeded" || host.Creates != 1 || p.feature(t) != DeliveredState {
		t.Fatalf("publication %+v creates %d", result, host.Creates)
	}
	records, err := publications(p.repository, p.stream)
	must(t, err)
	last := records[len(records)-1]
	if last.Upstream != p.s.cfg.Project.Upstream || last.PriorURL != p.pulls.Prs[0].URL {
		t.Fatalf("relationship %+v", last)
	}
	followups, err := followup.Read(p.repository, p.stream)
	must(t, err)
	if len(followups) != 0 {
		t.Fatal("maintenance created implementation units")
	}
	before := len(records)
	_, err = p.publisher().Apply(ctx, publish)
	must(t, err)
	records, err = publications(p.repository, p.stream)
	must(t, err)
	if len(records) != before || host.Creates != 1 {
		t.Fatal("retry duplicated delivery")
	}
}

// TestBaseRefreshRetryDoesNotDuplicateObservationAndPassStopsScheduling
// covers spec#3: once the retried Apply leaves the dependency's integration
// recorded, Pass schedules no further base-refresh, so the latest recorded
// base-refresh operation is still the one already applied.
func TestBaseRefreshRetryDoesNotDuplicateObservationAndPassStopsScheduling(t *testing.T) {
	t.Parallel()
	p := maintenanceFixture(t)
	ctx := context.Background()
	refresh := &baseRefresher{s: p.s, repository: p.repository}
	op := actionOperation(t, p.repository, p.stream, baseRefreshAction)
	p.reopen(t)
	refresh.repository = p.repository
	observed, err := refresh.Inspect(ctx, op)
	must(t, err)
	if observed.State != coreadapter.EffectCompleted {
		t.Fatal(observed)
	}
	_, err = refresh.Apply(ctx, op)
	must(t, err)
	_, rev, err := baseObservationAt(p.repository, p.stream)
	must(t, err)
	if rev != 1 {
		t.Fatalf("retry revision %d", rev)
	}
	jump(p.clock, p.s.now().Add(11*time.Minute))
	must(t, refresh.Pass(ctx))
	next := actionOperation(t, p.repository, p.stream, baseRefreshAction)
	if next.ID != op.ID {
		t.Fatal("base-refresh was scheduled again after integration was recorded")
	}
	_, rev, err = baseObservationAt(p.repository, p.stream)
	must(t, err)
	if rev != 1 {
		t.Fatalf("unchanged observation revision %d", rev)
	}
}
func TestDependentDeliveryMaintenanceGapKeepsImplementationTerminal(t *testing.T) {
	t.Parallel()
	p := maintenanceFixture(t)
	ctx := context.Background()
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	must(t, reviewer.Pass(ctx))
	op := actionOperation(t, p.repository, p.stream, deliveryReviewAction)
	in, err := decodeFinalReview(op)
	must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Integration exposes a gap", "findings": []any{map[string]any{"severity": "blocking", "criteria": []any{"spec#1"}, "finding": "resume behavior is missing"}}})
		return err
	})
	_, err = reviewer.Apply(ctx, op)
	must(t, err)
	report, reason, err := reviewer.finalGate(ctx, p.stream)
	must(t, err)
	if !report.Maintenance || reason != "" || len(reportBlocking(report)) != 1 || p.feature(t) != DeliveredState {
		t.Fatalf("maintenance gap: %+v reason %q state %s", report, reason, p.feature(t))
	}
	units, err := followup.Read(p.repository, p.stream)
	must(t, err)
	if len(units) != 0 {
		t.Fatal("maintenance gap reopened implementation")
	}
	must(t, p.publisher().Pass(ctx))
	ops, err := p.repository.Operations(p.stream)
	must(t, err)
	for _, op := range ops {
		if op.Operation.Action == publishUpstreamAction {
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
	must(t, repo.CreateWorkstream(ctx, child, p.s.now(), ownerActor))
	_, err := repo.SetWorkstreamBase(ctx, child, parent, 0, p.s.now())
	must(t, err)
	sealed, _, _, err := seal.Latest(repo, parent)
	must(t, err)
	publication := DeliveryPublication{Status: publicationOpened, Base: "main", Upstream: p.s.cfg.Project.Upstream, Fork: p.s.cfg.Project.Fork, Branch: sealed.Branch, Commit: sealed.Base.Commit, PullRequest: 7}
	data, err := json.Marshal(publication)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: 1, ID: publicationDocument, Revision: 1, Project: repo.Project(), Workstream: parent, At: p.s.now(), Actor: foremanActor, Cause: "delivery"}
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: publicationPath, Content: string(data)}}))
	host := p.s.options.PullRequests.(repositoryPulls)[p.s.cfg.Project.Upstream]
	host.Prs = []pulls.PullRequest{{Number: 7, Merged: true, Base: "main", Head: sealed.Branch, HeadRepository: p.s.cfg.Project.Fork, HeadCommit: sealed.Base.Commit}}
	demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "branch", "-D", sealed.Branch)
	ratified, err := shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, nil))
	must(t, err)
	h.Workstream, h.ID = child, shed.RatificationDocumentID(1)
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: shed.RatificationPath(1), Content: string(ratified)}}))
	must(t, p.s.baseWaitPass(ctx, repo))
	waiting, err := baseWaiting(repo, child)
	must(t, err)
	if !waiting {
		t.Fatal("missing branch did not park the dependent")
	}
	refresh := &baseRefresher{s: p.s, repository: repo}
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, child, baseRefreshAction)
	result := settleOperation(t, p.s, repo, child, op, refresh)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	must(t, p.s.baseWaitPass(ctx, repo))
	waiting, err = baseWaiting(repo, child)
	must(t, err)
	if waiting {
		t.Fatal("integrated parent left sealing parked")
	}
	observed, _, err := baseObservationAt(repo, child)
	must(t, err)
	if observed.Unavailable || observed.Base.Workstream != "" || observed.Base.Commit == "" {
		t.Fatalf("integration observation %+v", observed)
	}
}
