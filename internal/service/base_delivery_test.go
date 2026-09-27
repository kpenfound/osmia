package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type repositoryPulls map[string]*fakePulls

func (r repositoryPulls) Find(ctx context.Context, repository, head, branch string) ([]pulls.PullRequest, error) {
	return r[repository].Find(ctx, repository, head, branch)
}
func (r repositoryPulls) Create(ctx context.Context, repository string, n pulls.New) (pulls.PullRequest, error) {
	return r[repository].Create(ctx, repository, n)
}

func maintenanceFixture(t *testing.T) *publicationFixture {
	t.Helper()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	upstream := p.s.cfg.Project.Upstream
	// Record a completed delivery on the fork, then configure the canonical
	// upstream for the integration check and second publication.
	p.s.cfg.Project.Upstream = p.s.cfg.Project.Fork
	p.approve(t, nil)
	result, err := p.publisher().Apply(ctx, p.request(t))
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	p.s.cfg.Project.Upstream = upstream
	parent := config.WorkstreamID("w_fffffffffffffffffffffffffffffff0")
	repo := p.repository
	must(t, repo.CreateWorkstream(ctx, parent, p.s.now(), trace.Actor{Kind: "owner", ID: "owner"}))
	sealed, doc, _, err := seal.Latest(repo, p.stream)
	must(t, err)
	sealed.Branch = featureBranch(parent)
	data, err := seal.Encode(sealed)
	must(t, err)
	doc.Workstream, doc.Content, doc.Revision = parent, string(data), 1
	must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
	h := doc.Header
	h.Schema, h.ID = "osmia.trace.transition", "parent-delivered"
	_, err = repo.SetFeatureState(ctx, h, DeliveredState, "Parent delivered before upstream integration")
	must(t, err)
	demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "branch", sealed.Branch, sealed.Base.Commit)
	base := trace.WorkstreamBase{Workstream: p.stream, Base: parent, Revision: 1}
	data, err = json.Marshal(base)
	must(t, err)
	doc.Workstream, doc.ID, doc.Path, doc.Content = p.stream, "workstream-base", "base.json", string(data)
	must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
	// A second upstream commit forces maintenance to move the reviewed branch.
	advanceUpstream(t, p.shedFixture, map[string]string{"INTEGRATED.md": "dependency is integrated\n"})
	host := &fakePulls{fork: func() string { tip, _ := p.forkBranch(t); return tip }}
	p.s.options.PullRequests = repositoryPulls{upstream: host, p.s.cfg.Project.Fork: p.pulls}
	refresh := &baseRefresher{s: p.s, repository: repo}
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, p.stream, baseRefreshAction)
	result, err = refresh.Apply(ctx, op)
	must(t, err)
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	observed, rev, err := baseObservationAt(repo, p.stream)
	must(t, err)
	if rev != 1 || observed.Unavailable || observed.Base.Workstream != "" {
		t.Fatalf("integration %+v %d", observed, rev)
	}
	return p
}

func actionOperation(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, action string) coreadapter.Operation {
	t.Helper()
	ops, err := repo.Operations(stream)
	must(t, err)
	var found coreadapter.Operation
	var at time.Time
	for _, op := range ops {
		if op.Operation.Action == action && op.Transition.At.After(at) {
			found = op.Operation
			at = op.Transition.At
		}
	}
	if found.ID == "" {
		t.Fatalf("no %s operation", action)
	}
	return found
}

func TestDependentDeliveryMaintenanceReviewsApprovesAndReconcilesUpstream(t *testing.T) {
	p := maintenanceFixture(t)
	ctx := context.Background()
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
	host.lose = true
	if _, err := p.publisher().Apply(ctx, publish); err == nil {
		t.Fatal("expected lost hosting response")
	}
	p.reopen(t)
	result, err = p.publisher().Apply(ctx, publish)
	must(t, err)
	if result.Outcome != "succeeded" || host.creates != 1 || p.feature(t) != DeliveredState {
		t.Fatalf("publication %+v creates %d", result, host.creates)
	}
	records, err := publications(p.repository, p.stream)
	must(t, err)
	last := records[len(records)-1]
	if last.Upstream != p.s.cfg.Project.Upstream || last.PriorURL != p.pulls.prs[0].URL {
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
	if len(records) != before || host.creates != 1 {
		t.Fatal("retry duplicated delivery")
	}
}

func TestBaseRefreshRetryDoesNotDuplicateObservation(t *testing.T) {
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
	jump(p.clock, p.s.now().Add(2*time.Minute))
	must(t, refresh.Pass(ctx))
	next := actionOperation(t, p.repository, p.stream, baseRefreshAction)
	if next.ID == op.ID {
		t.Fatal("next integration check not scheduled")
	}
	_, err = refresh.Apply(ctx, next)
	must(t, err)
	_, rev, err = baseObservationAt(p.repository, p.stream)
	must(t, err)
	if rev != 1 {
		t.Fatalf("unchanged observation revision %d", rev)
	}
}

func TestDependentDeliveryMaintenanceGapKeepsImplementationTerminal(t *testing.T) {
	p := maintenanceFixture(t)
	ctx := context.Background()
	reviewer := &finalReviewer{s: p.s, repository: p.repository}
	must(t, reviewer.Pass(ctx))
	op := actionOperation(t, p.repository, p.stream, deliveryReviewAction)
	in, err := decodeFinalReview(op)
	must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Integration exposes a gap", "criteria": []any{map[string]any{"criterion": "spec#1", "gap": "resume behavior is missing"}, map[string]any{"criterion": "spec#2", "evidence": "dedupe.go"}}})
		return err
	})
	_, err = reviewer.Apply(ctx, op)
	must(t, err)
	report, reason, err := reviewer.finalGate(ctx, p.stream)
	must(t, err)
	if !report.Maintenance || !strings.Contains(reason, "repairs require a new workstream") || p.feature(t) != DeliveredState {
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
	publication := DeliveryPublication{Status: publicationOpened, Upstream: p.s.cfg.Project.Upstream, Fork: p.s.cfg.Project.Fork, Branch: sealed.Branch, Commit: sealed.Base.Commit, PullRequest: 7}
	data, err := json.Marshal(publication)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: 1, ID: publicationDocument, Revision: 1, Project: repo.Project(), Workstream: parent, At: p.s.now(), Actor: foremanActor, Cause: "delivery"}
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: publicationPath, Content: string(data)}}))
	host := p.s.options.PullRequests.(repositoryPulls)[p.s.cfg.Project.Upstream]
	host.prs = []pulls.PullRequest{{Number: 7, Merged: true, Head: sealed.Branch, HeadRepository: p.s.cfg.Project.Fork, HeadCommit: sealed.Base.Commit}}
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
	result, err := refresh.Apply(ctx, op)
	must(t, err)
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
