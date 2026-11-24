package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kpenfound/osmia/internal/execution"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newCrossRepoEdgeDependent(t *testing.T) (f *shedFixture, stream config.WorkstreamID, repository *trace.Repository, report api.FinalReport, repo2 *trace.Repository, upstream config.WorkstreamID) {
	t.Helper()
	f, stream, repository, report = deliveryFixtureWith(t, "")
	repo2, upstream = fixturekit.CrossRepoEdgeDependent(t, f.s.cfg, f.clone, repository, stream, f.s.now, intake.OwnerActor, intake.RegistrationActor, intake.FeatureBranch, func(p config.Project, repo *trace.Repository) {
		f.s.setSole(&activeProject{id: repository.Project(), repository: repository})
		f.s.mu.Lock()
		f.s.cfg.Projects = append(f.s.cfg.Projects, p)
		f.s.projects = append(f.s.projects, &activeProject{id: p.ID, repository: repo})
		f.s.mu.Unlock()
	}, f.s.DraftWorkgroup)
	return f, stream, repository, report, repo2, upstream
}

func recordUpstreamOwnPullRequest(t *testing.T, s *Service, repo2 *trace.Repository, upstream config.WorkstreamID, publication api.DeliveryPublication) {
	t.Helper()
	fixturekit.UpstreamOwnPullRequest(t, repo2, upstream, publication, s.now(), execution.ForemanActor, delivery.PublicationDocument, delivery.PublicationPath)
}

func TestCrossRepoMergeOrderEdgeDraftThenReadyAfterMergeAndFreshApproval(t *testing.T) {
	t.Parallel()
	exerciseCrossRepoMergeOrder(t, false, false)
}
func TestCrossRepoMergeOrderPostMergeRepairAndRereview(t *testing.T) {
	t.Parallel()
	exerciseCrossRepoMergeOrder(t, true, false)
}
func TestReadyWorkgroupDependentSameRepoMaintenanceGapRequiresNewWorkstream(t *testing.T) {
	t.Parallel()
	exerciseCrossRepoMergeOrder(t, false, true)
}
func exerciseCrossRepoMergeOrder(t *testing.T, repair, baseMaintenance bool) {
	t.Helper()
	ctx := context.Background()
	f, stream, repository, report, repo2, upstream := newCrossRepoEdgeDependent(t)
	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	fixturekit.ConfigureDeliverySigner(t, f.clone)
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	fixturekit.DemoGit(t, home, "init", "--quiet", "--bare", fork)
	fixturekit.DemoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	pull := &fixturekit.FakePulls{SyncHead: true, Fork: func() string {
		out := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+intake.FeatureBranch(stream)))
		return out
	}}
	f.s.mu.Lock()
	f.s.options.PullRequests = pull
	f.s.mu.Unlock()

	var parent config.WorkstreamID
	if baseMaintenance {
		parent, report = seedEdgeDependentBase(t, f, repository, stream, report)
	}

	upstreamPullRequest := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "owner/other", Fork: "owner/other-fork", Branch: intake.FeatureBranch(upstream), Commit: "d1e2f3a4b5c6d1e2f3a4b5c6d1e2f3a4b5c6d1e2", PullRequest: 77, URL: "https://example.invalid/owner/other/pull/77"}
	recordUpstreamOwnPullRequest(t, f.s, repo2, upstream, upstreamPullRequest)
	pull.Mu.Lock()
	pull.Prs = append(pull.Prs, pulls.PullRequest{Number: upstreamPullRequest.PullRequest, URL: upstreamPullRequest.URL, State: "open", HeadRepository: upstreamPullRequest.Fork, Head: upstreamPullRequest.Branch, HeadCommit: upstreamPullRequest.Commit, Base: "main"})
	pull.Mu.Unlock()

	fixturekit.Must(t, f.s.lifecycle().EdgeResolverPass(ctx, repository))
	provider := f.s.Context()
	assertContext := func(merged bool) {
		t.Helper()
		fixturekit.Must(t, (&workgroupResolver{s: f.s, repository: repository}).Pass(ctx))
		inbox, apiFault := f.s.inbox(ctx)
		if apiFault != nil {
			t.Fatal(apiFault)
		}
		for _, entry := range inbox.Entries {
			if entry.Kind != api.InboxRatification && entry.Kind != api.InboxDelivery {
				t.Fatalf("dependency requested an extra owner action: %+v", entry)
			}
		}
		view, err := provider.Assemble(ctx, repository.Project(), bundle.Scope{Role: intake.MasonRole, Workstream: stream})
		fixturekit.Must(t, err)
		if view.ProjectSpec == nil || len(view.ProjectSpec.Contracts) != 1 || len(view.Dependencies) != 1 || view.Dependencies[0].Merged != merged {
			t.Fatalf("dependent context: %+v", view)
		}
		if text := view.Render(); !strings.Contains(text, "Use the upstream interface.") || !strings.Contains(text, "source: "+trace.ProjectSpecsPath+" (record "+trace.ProjectSpecsDocument) {
			t.Fatalf("dependent turn omits the ratified contract or its source: %s", text)
		}
		if !merged && view.Dependencies[0].BuildBranch == "" {
			t.Fatal("dependent has no upstream branch or commit to build against")
		}
		chief, _, bundleStream, closeChief, err := f.s.lifecycle().ChiefRepository(ctx, repository, stream)
		fixturekit.Must(t, err)
		context, err := service.TestChiefContext(ctx, provider, chief, closeChief, repository.Project(), bundleStream)
		fixturekit.Must(t, err)
		if !strings.Contains(context, "Use the upstream interface.") || !strings.Contains(context, "Downstream depends on upstream.") || !strings.Contains(context, "source: "+trace.ProjectSpecsPath+" (record "+trace.ProjectSpecsDocument) {
			t.Fatal("shared chief lost the ratified project contract")
		}
	}
	assertContext(false)

	pub := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: report, fork: fork, pulls: pull}
	pub.publishDraft(t)
	approval := pub.approve(t, nil)
	if !strings.Contains(approval.Description, "owner/other#77") {
		t.Fatalf("the drafted description does not link the upstream pull request: %q", approval.Description)
	}
	op := pub.request(t)
	result, err := pub.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	if got := pub.feature(t); got != intake.DeliveredState {
		t.Fatalf("the dependent is %s, want delivered: the approval is applied and the pull request stays a draft", got)
	}
	pull.Mu.Lock()
	if len(pull.Prs) != 2 || !pull.Prs[1].Draft || pull.Readies != 0 || !strings.Contains(pull.Prs[1].Body, "owner/other#77") {
		t.Fatalf("the dependent's own pull request is not a draft linking the dependency: %+v, ready calls %d", pull.Prs, pull.Readies)
	}
	pull.Mu.Unlock()
	if ready, recorded, err := delivery.ReadyAt(repository, stream); err != nil || !recorded || !ready.Draft || ready.Approval != 1 {
		t.Fatalf("the approval of the draft is not recorded: %+v, %v", ready, err)
	}
	firstPublication, found, err := delivery.CurrentOwnPublication(repository, stream)
	fixturekit.Must(t, err)
	if !found || !strings.Contains(fixturekit.DemoGit(t, home, "-C", f.clone, "cat-file", "-p", firstPublication.Commit), "gpgsig") {
		t.Fatalf("draft push lacks signing: %+v", firstPublication)
	}

	pull.Mu.Lock()
	pull.Prs[0].Merged = true
	pull.Prs[0].MergeCommit = "aabbccddeeff0011223344556677889900aabbccdd"
	pull.Mu.Unlock()
	f.clock.Mu.Lock()
	f.clock.At = f.clock.At.Add(11 * time.Minute)
	f.clock.Mu.Unlock()
	fixturekit.Must(t, f.s.lifecycle().EdgeResolverPass(ctx, repository))
	if pending, err := lifecycle.UpstreamDeliveryPending(repository, f.s.about(repository), stream); err != nil || !pending {
		t.Fatalf("the upstream merge was not recognised as pending maintenance: %v %v", pending, err)
	}

	a := &finalReviewer{s: f.s, repository: repository}
	fixturekit.Must(t, delivery.RecordOwnPullRequestOutcome(ctx, repo2, f.s.now(), upstream, "fixture", pull.Prs[0].Number, delivery.OwnPullRequestOutcomeMerged))
	if done, err := lifecycle.Resolved(repo2, upstream); err != nil || !done {
		t.Fatalf("merged upstream is not resolved: %v, %v", done, err)
	}
	assertContext(true)
	if done, err := lifecycle.Resolved(repository, stream); err != nil || done {
		t.Fatalf("dependent resolved before its update and approval: %v, %v", done, err)
	}
	updates, err := followup.Read(repository, stream)
	fixturekit.Must(t, err)
	if len(updates) != 1 || !strings.Contains(updates[0].Unit.Task, pull.Prs[0].MergeCommit) {
		t.Fatalf("merged dependency update units: %+v", updates)
	}
	fixturekit.Must(t, f.s.lifecycle().EdgeResolverPass(ctx, repository))
	retried, err := followup.Read(repository, stream)
	fixturekit.Must(t, err)
	if len(retried) != 1 {
		t.Fatalf("reconciling the merge duplicated the update: %+v", retried)
	}
	if _, reason, err := a.finalGate(ctx, stream); err != nil || reason == "" {
		t.Fatalf("old review authorizes the post-merge update: %q, %v", reason, err)
	}
	build := &masons{s: f.s, cfg: f.s.about(repository), repository: repository}
	if _, admitted, err := build.read(stream); err != nil || !admitted {
		t.Fatalf("post-merge update is stranded: %v, %v", admitted, err)
	}
	mergeDirectly(t, f, repository, stream, updates[0].Unit.ID, map[string]string{"dependency.txt": pull.Prs[0].MergeCommit + "\n"})
	runReview := func(gap bool) api.FinalReport {
		t.Helper()
		f.clock.Mu.Lock()
		f.clock.At = f.clock.At.Add(time.Second)
		f.clock.Mu.Unlock()
		fixturekit.Must(t, a.Pass(ctx))
		op := fixturekit.ActionOperation(t, repository, stream, lifecycle.DeliveryReviewAction)
		in, err := delivery.DecodeFinalReview(op)
		fixturekit.Must(t, err)
		f.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
			findings := []any{}
			if gap {
				findings = append(findings, map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "TestDedupe fails with the merged dependency; repair dedupe.go within the slice"})
			}
			_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Dependency updated", "findings": findings})
			return err
		})
		result := settleOperation(t, f.s, repository, stream, op, a)
		if result.Outcome != "succeeded" {
			t.Fatal(result)
		}
		fresh, found, err := delivery.LatestFinalReport(repository, stream)
		fixturekit.Must(t, err)
		if !found || fresh.EdgeResolved == "" || fresh.Commit == report.Commit {
			t.Fatalf("fresh review did not read the dependency update: %+v", fresh)
		}
		assertContext(true)
		return fresh
	}
	fresh := runReview(repair)
	if repair {
		units, err := followup.Read(repository, stream)
		fixturekit.Must(t, err)
		if len(units) != 2 || units[1].Criterion != "spec#2" {
			t.Fatalf("repair not queued on the same workstream: %+v", units)
		}
		if _, admitted, err := build.read(stream); err != nil || !admitted {
			t.Fatalf("repair is stranded: %v, %v", admitted, err)
		}
		if _, reason, err := a.finalGate(ctx, stream); err != nil || reason == "" || strings.Contains(reason, "new workstream") {
			t.Fatalf("in-scope repair gate: %q, %v", reason, err)
		}
		mergeDirectly(t, f, repository, stream, units[1].Unit.ID, map[string]string{"dedupe.go": "package demo\n\nfunc Dedupe() bool { return true }\n"})
		fresh = runReview(false)
	}

	if pull.Readies != 0 {
		t.Fatal("the dependent's pull request was marked ready before any fresh approval")
	}
	freshApproval := pub.approve(t, nil)
	if freshApproval.Review != fresh.Review {
		t.Fatalf("the fresh approval read review %d, want %d", freshApproval.Review, fresh.Review)
	}
	fixturekit.Must(t, pub.publisher().Pass(ctx))
	op2 := fixturekit.ActionOperation(t, repository, stream, lifecycle.PublishUpstreamAction)
	result2, err := pub.publisher().Apply(ctx, op2)
	if err != nil || result2.Outcome != "succeeded" {
		t.Fatalf("the fresh publication %+v: %v", result2, err)
	}
	publication, found, err := delivery.CurrentOwnPublication(repository, stream)
	fixturekit.Must(t, err)
	if !found || publication.Approval != 2 || publication.Commit == firstPublication.Commit || !strings.Contains(fixturekit.DemoGit(t, home, "-C", f.clone, "cat-file", "-p", publication.Commit), "gpgsig") {
		t.Fatalf("ready publication lacks fresh owner approval and signing: %+v", publication)
	}
	if got := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.clone, "show", publication.Commit+":dependency.txt")); got != pull.Prs[0].MergeCommit {
		t.Fatalf("published dependency reference is %q, want the merged commit", got)
	}
	pull.Mu.Lock()
	draft, readies, creates := pull.Prs[1].Draft, pull.Readies, pull.Creates
	pull.Mu.Unlock()
	if draft {
		t.Fatal("the dependent's pull request was not marked ready after the fresh delivery approval")
	}
	if readies != 1 {
		t.Fatalf("marking ready happened %d times, want exactly once", readies)
	}
	if creates != 1 {
		t.Fatalf("the dependent's pull request was opened %d times, want exactly once (never a second pull request)", creates)
	}
	if baseMaintenance {
		exerciseReadyDependentBaseGap(t, pub, parent)
	}
}

func seedEdgeDependentBase(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, report api.FinalReport) (config.WorkstreamID, api.FinalReport) {
	t.Helper()
	ctx := context.Background()
	parent := config.WorkstreamID("w_fffffffffffffffffffffffffffffff0")
	fixturekit.Must(t, repo.CreateWorkstream(ctx, parent, f.s.now(), intake.OwnerActor))
	sealed, doc, _, err := seal.Latest(repo, stream)
	fixturekit.Must(t, err)
	fixturekit.DemoGit(t, filepath.Dir(f.clone), "-C", f.clone, "branch", intake.FeatureBranch(parent), sealed.Base.Commit)
	parentTip := moveFeature(t, f, parent, map[string]string{"BASE.md": "same-repo dependency\n"})
	fixturekit.DemoGit(t, filepath.Dir(f.clone), "-C", f.clone, "push", "--quiet", "origin", parentTip+":refs/heads/"+intake.FeatureBranch(parent))
	parentSeal := sealed
	parentSeal.Branch = intake.FeatureBranch(parent)
	data, err := seal.Encode(parentSeal)
	fixturekit.Must(t, err)
	parentDoc := doc
	parentDoc.Workstream, parentDoc.Content, parentDoc.Revision = parent, string(data), 1
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{parentDoc}))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "parent-delivered", Revision: 1, Project: repo.Project(), Workstream: parent, At: f.s.now(), Actor: execution.ForemanActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, intake.DeliveredState, "Same-repo base delivered before integration")
	fixturekit.Must(t, err)
	base := trace.WorkstreamBase{Workstream: stream, Base: parent, Revision: 1}
	data, err = json.Marshal(base)
	fixturekit.Must(t, err)
	baseDoc := doc
	baseDoc.ID, baseDoc.Path, baseDoc.Content, baseDoc.Revision = "workstream-base", "base.json", string(data), 1
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{baseDoc}))

	g, err := intake.FeatureWorkspaces(f.s.about(repo), repo).Of(stream)
	fixturekit.Must(t, err)
	w, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Branch: intake.FeatureBranch(stream)})
	fixturekit.Must(t, err)
	tip, conflicts, err := g.ReplayFrom(ctx, sealed.Base.Commit, report.Commit, parentTip, f.s.now())
	fixturekit.Must(t, err)
	if len(conflicts) != 0 {
		t.Fatalf("seeding base conflicts: %v", conflicts)
	}
	fixturekit.Must(t, g.Move(ctx, w.(workspace.Worktree), report.Commit, tip))
	sealed.Base = seal.Base{Remote: ".", Branch: intake.FeatureBranch(parent), Commit: parentTip, Workstream: parent}
	data, err = seal.Encode(sealed)
	fixturekit.Must(t, err)
	doc.Revision++
	doc.Content = string(data)
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
	report.Commit, report.Upstream = tip, &sealed.Base
	data, err = json.Marshal(report)
	fixturekit.Must(t, err)
	h.Schema, h.ID, h.Workstream, h.Revision = "osmia.trace.document", delivery.FinalReportDocument, stream, 2
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: delivery.FinalReportPath, Content: string(data)}}))
	return parent, report
}
func exerciseReadyDependentBaseGap(t *testing.T, p *publicationFixture, parent config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	repo, stream := p.repository, p.stream
	if pending, err := lifecycle.WorkgroupEdgePending(repo, stream); err != nil || pending {
		t.Fatalf("ready publication retains edge repair permission: %v, %v", pending, err)
	}
	if resolved, err := lifecycle.WorkgroupMergeOrderResolved(repo, stream); err != nil || resolved == "" {
		t.Fatalf("ready publication lost historical edge resolution: %q, %v", resolved, err)
	}
	before, err := followup.Read(repo, stream)
	fixturekit.Must(t, err)
	upstreamTip := advanceUpstream(t, p.shedFixture, map[string]string{"BASE.md": "same-repo dependency\n", "INTEGRATED.md": "base is integrated\n"})
	parentTip := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(p.clone), "-C", p.clone, "rev-parse", intake.FeatureBranch(parent)))
	merged := api.DeliveryMerge{Branch: intake.FeatureBranch(parent), Commit: parentTip, Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: upstreamTip}}
	data, err := json.Marshal(merged)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.MergeDocument, Revision: 1, Project: repo.Project(), Workstream: parent, At: p.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}
	fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: delivery.MergePath, Content: string(data)}}))
	fixturekit.Jump(p.clock, p.s.now().Add(11*time.Minute))
	refresh := &baseRefresher{s: p.s, repository: repo}
	fixturekit.Must(t, refresh.Pass(ctx))
	op := fixturekit.ActionOperation(t, repo, stream, lifecycle.BaseRefreshAction)
	if result := settleOperation(t, p.s, repo, stream, op, refresh); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	if pending, err := lifecycle.UpstreamDeliveryPending(repo, p.s.about(repo), stream); err != nil || !pending {
		t.Fatalf("later base integration did not schedule maintenance: %v, %v", pending, err)
	}
	reviewer := &finalReviewer{s: p.s, repository: repo}
	fixturekit.Must(t, reviewer.Pass(ctx))
	op = fixturekit.ActionOperation(t, repo, stream, lifecycle.DeliveryReviewAction)
	in, err := delivery.DecodeFinalReview(op)
	fixturekit.Must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Base integration exposes a gap", "findings": []any{map[string]any{"severity": "blocking", "criteria": []any{"spec#1"}, "finding": "resume behavior is missing after base integration"}}})
		return err
	})
	if result := settleOperation(t, p.s, repo, stream, op, reviewer); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	report, reason, err := reviewer.finalGate(ctx, stream)
	fixturekit.Must(t, err)
	if !report.Maintenance || report.EdgeResolved == "" || reason != "" || len(delivery.ReportBlocking(report)) != 1 || p.feature(t) != intake.DeliveredState {
		t.Fatalf("later maintenance gap: %+v reason %q state %s", report, reason, p.feature(t))
	}
	if body := intakefixture.NoticeOf(t, repo, stream, fmt.Sprintf("final-review-%d-reviewed", in.Review)); !strings.Contains(body, "move the workstream to building with move_workstream") {
		t.Fatalf("the chief heard %q", body)
	}
	after, err := followup.Read(repo, stream)
	fixturekit.Must(t, err)
	if len(after) != len(before) {
		t.Fatalf("later maintenance created in-place repairs: before %+v, after %+v", before, after)
	}
	build := &masons{s: p.s, cfg: p.s.about(repo), repository: repo}
	if _, admitted, err := build.read(stream); err != nil || admitted {
		t.Fatalf("ready workgroup dependent admitted implementation: %v, %v", admitted, err)
	}
	land := &foreman{masons: build}
	if reason, err := land.current(ctx, stream, execution.LandInput{}, service.UnitReviewResult{}); err != nil || !strings.Contains(reason, "not building or assembled") {
		t.Fatalf("later maintenance admitted landing: %q, %v", reason, err)
	}
	presented, apiFault := p.s.deliveryPresentation(ctx, string(stream))
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	decision := api.DeliveryDecision{Review: report.Review, ReviewRevision: presented.ReviewRevision, Commit: report.Commit, DraftHash: presented.DraftHash}
	if _, apiFault := p.s.approveDelivery(ctx, string(stream), decision); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "approve with accept_blocking") {
		t.Fatalf("later maintenance gap accepted delivery approval: %v", apiFault)
	}
	fixturekit.Must(t, p.publisher().Pass(ctx))
	latest := fixturekit.ActionOperation(t, repo, stream, lifecycle.PublishUpstreamAction)
	publication, found, err := delivery.CurrentOwnPublication(repo, stream)
	fixturekit.Must(t, err)
	if !found || latest.ID != publication.Operation || publication.Approval != 2 || publication.Draft {
		t.Fatalf("later maintenance authorized publication: %+v, %+v", latest, publication)
	}
}

func TestWorkgroupDependentPostMergeGapRepairsInScopeNeverBlocksLikeSameRepoMaintenance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository, report, repo2, upstream := newCrossRepoEdgeDependent(t)
	a := &finalReviewer{s: f.s, repository: repository}

	_, err := repo2.SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver", Revision: 1, Project: repo2.Project(), Workstream: upstream, At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}, intake.DeliveredState, "fixture delivery")
	fixturekit.Must(t, err)
	mergeRecord := api.DeliveryMerge{Branch: intake.FeatureBranch(upstream), Commit: "e1f2a3b4c5d6e1f2a3b4c5d6e1f2a3b4c5d6e1f2", Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: "e1f2a3b4c5d6e1f2a3b4c5d6e1f2a3b4c5d6e1f2"}}
	mergeData, err := json.Marshal(mergeRecord)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo2.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.MergeDocument, Revision: 1, Project: repo2.Project(), Workstream: upstream, At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}, Path: delivery.MergePath, Content: string(mergeData)}}))
	fixturekit.Must(t, f.s.lifecycle().EdgeResolverPass(ctx, repository))

	in, sealed, err := a.governing(ctx, stream)
	fixturekit.Must(t, err)
	if in.EdgeResolved == "" {
		t.Fatalf("governing inputs do not yet show the upstream merge resolved: %+v", in)
	}

	gapped := api.FinalReport{Review: report.Review + 1, Outcome: delivery.FinalReviewed, Branch: intake.FeatureBranch(stream), Commit: in.Commit, Seal: in.Seal, SpecHash: sealed.SpecHash, Spec: in.Spec, Plan: in.Plan, Charter: in.Charter, Upstream: &sealed.Base, Maintenance: in.Maintenance, EdgeResolved: in.EdgeResolved, Summary: report.Summary, Criteria: []api.FinalCriterion{
		{Criterion: "spec#1", Evidence: "resume.go and TestResume"},
		{Criterion: "spec#2", Gap: "TestDedupe now fails after updating the dependency reference"},
	}}
	data, err := json.Marshal(gapped)
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalReportDocument, Revision: 2, Project: repository.Project(), Workstream: stream, At: f.s.now(), Actor: delivery.FinalReviewActor, Cause: "final-review"}, Path: delivery.FinalReportPath, Content: string(data)}}))

	before, err := repository.Inbox()
	fixturekit.Must(t, err)
	added, found, err := a.followup(stream, gapped, 2)
	fixturekit.Must(t, err)
	if !found || added.Criterion != "spec#2" || added.Gap != "spec#2: "+gapped.Criteria[1].Gap || added.Unit.ID == "" {
		t.Fatalf("follow-up unit for the post-merge gap: %+v", added)
	}
	after, err := repository.Inbox()
	fixturekit.Must(t, err)
	if len(after) != len(before) {
		t.Fatalf("computing the in-scope repair's follow-up unit added %d inbox entries", len(after)-len(before))
	}
}
