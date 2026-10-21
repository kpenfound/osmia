package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newCrossRepoEdgeDependent builds a dependent workstream whose build,
// seal and first clean final review are exactly deliveryFixtureWith's
// (one project, service stopped afterward so nothing but the test itself
// drives any pass), then registers a second project directly on the same,
// stopped service for the upstream workgroup member and records a
// cross-repo workgroup whose ratified project spec carries a merge_order
// edge from upstream to the dependent. It records no merge: callers decide
// when, mirroring the two different moments the two tests below need it.
func newCrossRepoEdgeDependent(t *testing.T) (f *shedFixture, stream config.WorkstreamID, repository *trace.Repository, report FinalReport, repo2 *trace.Repository, upstream config.WorkstreamID) {
	t.Helper()
	f, stream, repository, report = deliveryFixtureWith(t, "")
	repo2, upstream = fixturekit.CrossRepoEdgeDependent(t, f.s.cfg, f.clone, repository, stream, f.s.now, ownerActor, registrationActor, featureBranch, func(p config.Project, repo *trace.Repository) {
		f.s.setSole(&activeProject{id: repository.Project(), repository: repository})
		f.s.mu.Lock()
		f.s.cfg.Projects = append(f.s.cfg.Projects, p)
		f.s.projects = append(f.s.projects, &activeProject{id: p.ID, repository: repo})
		f.s.mu.Unlock()
	}, f.s.DraftWorkgroup)
	return f, stream, repository, report, repo2, upstream
}

// recordUpstreamOwnPullRequest records, directly on upstream's own trace,
// that its feature branch is delivered through an open, unmerged pull
// request: the identity a dependent's draft links while the merge-order
// edge stays unresolved (spec#12).
func recordUpstreamOwnPullRequest(t *testing.T, s *Service, repo2 *trace.Repository, upstream config.WorkstreamID, publication DeliveryPublication) {
	t.Helper()
	fixturekit.UpstreamOwnPullRequest(t, repo2, upstream, publication, s.now(), foremanActor, publicationDocument, publicationPath)
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
	checkCalls := 0
	f.s.options.reviewChecks = checkFunc(func(_ context.Context, directory string) (CheckResult, error) {
		checkCalls++
		dependency, err := os.ReadFile(filepath.Join(directory, "dependency.txt"))
		if err != nil || strings.TrimSpace(string(dependency)) != "aabbccddeeff0011223344556677889900aabbccdd" {
			return CheckResult{}, fmt.Errorf("checks missed merged dependency reference: %q, %v", dependency, err)
		}
		if repair && checkCalls == 1 {
			return CheckResult{ExitCode: 1, Output: failedReport("dag://?check=go/packages/tests/test&go-test=TestDedupe") + "TestDedupe fails after the dependency update\n"}, nil
		}
		return CheckResult{Output: "dependency checks pass"}, nil
	})

	// The dependent's own fork and a fake pull-request host shared by both
	// repos, set up directly (not through newPublicationFixtureWith, which
	// would also touch the single-project Landing convenience field that
	// must stay untouched once a second project is registered).
	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	configureDeliverySigner(t, f.clone)
	must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	demoGit(t, home, "init", "--quiet", "--bare", fork)
	demoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	pull := &fakePulls{SyncHead: true, Fork: func() string {
		out := strings.TrimSpace(demoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+featureBranch(stream)))
		return out
	}}
	f.s.mu.Lock()
	f.s.options.PullRequests = pull
	f.s.mu.Unlock()

	var parent config.WorkstreamID
	if baseMaintenance {
		parent, report = seedEdgeDependentBase(t, f, repository, stream, report)
	}

	// Upstream already has its own open, unmerged pull request.
	upstreamPullRequest := DeliveryPublication{Status: publicationOpened, Upstream: "owner/other", Fork: "owner/other-fork", Branch: featureBranch(upstream), Commit: "d1e2f3a4b5c6d1e2f3a4b5c6d1e2f3a4b5c6d1e2", PullRequest: 77, URL: "https://example.invalid/owner/other/pull/77"}
	recordUpstreamOwnPullRequest(t, f.s, repo2, upstream, upstreamPullRequest)
	pull.Mu.Lock()
	pull.Prs = append(pull.Prs, pulls.PullRequest{Number: upstreamPullRequest.PullRequest, URL: upstreamPullRequest.URL, State: "open", HeadRepository: upstreamPullRequest.Fork, Head: upstreamPullRequest.Branch, HeadCommit: upstreamPullRequest.Commit, Base: "main"})
	pull.Mu.Unlock()

	must(t, f.s.edgeResolverPass(ctx, repository))
	provider := f.s.Context()
	assertContext := func(merged bool) {
		t.Helper()
		must(t, (&workgroupResolver{s: f.s, repository: repository}).Pass(ctx))
		inbox, api := f.s.inbox(ctx)
		if api != nil {
			t.Fatal(api)
		}
		for _, entry := range inbox.Entries {
			if entry.Kind != InboxRatification && entry.Kind != InboxDelivery {
				t.Fatalf("dependency requested an extra owner action: %+v", entry)
			}
		}
		view, err := provider.Assemble(ctx, repository.Project(), bundle.Scope{Role: masonRole, Workstream: stream})
		must(t, err)
		if view.ProjectSpec == nil || len(view.ProjectSpec.Contracts) != 1 || len(view.Dependencies) != 1 || view.Dependencies[0].Merged != merged {
			t.Fatalf("dependent context: %+v", view)
		}
		if text := view.Render(); !strings.Contains(text, "Use the upstream interface.") || !strings.Contains(text, "source: "+trace.ProjectSpecsPath+" (record "+trace.ProjectSpecsDocument) {
			t.Fatalf("dependent turn omits the ratified contract or its source: %s", text)
		}
		if !merged && view.Dependencies[0].BuildBranch == "" {
			t.Fatal("dependent has no upstream branch or commit to build against")
		}
		chief, _, bundleStream, closeChief, err := f.s.chiefRepository(ctx, repository, stream)
		must(t, err)
		context, err := chiefContext(ctx, provider, chief, closeChief, repository.Project(), bundleStream)
		must(t, err)
		if !strings.Contains(context, "Use the upstream interface.") || !strings.Contains(context, "Downstream depends on upstream.") || !strings.Contains(context, "source: "+trace.ProjectSpecsPath+" (record "+trace.ProjectSpecsDocument) {
			t.Fatal("shared chief lost the ratified project contract")
		}
	}
	assertContext(false)

	// The dependent's own delivery approval: the first owner prompt.
	pub := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: report, fork: fork, pulls: pull}
	approval := pub.approve(t, nil)
	if !strings.Contains(approval.Description, "owner/other#77") {
		t.Fatalf("the drafted description does not link the upstream pull request: %q", approval.Description)
	}
	op := pub.request(t)
	result, err := pub.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	if got := pub.feature(t); got != DeliveredState {
		t.Fatalf("the dependent is %s, want delivered: a draft pull request is still a published pull request", got)
	}
	pull.Mu.Lock()
	if len(pull.Prs) != 2 || !pull.Prs[1].Draft {
		t.Fatalf("the dependent's own pull request did not open as a draft linking the dependency: %+v", pull.Prs)
	}
	pull.Mu.Unlock()
	firstPublication, found, err := currentOwnPublication(repository, stream)
	must(t, err)
	if !found || firstPublication.Approval != 1 || !strings.Contains(demoGit(t, home, "-C", f.clone, "cat-file", "-p", firstPublication.Commit), "gpgsig") {
		t.Fatalf("draft publication lacks owner approval and signing: %+v", firstPublication)
	}

	// Simulate the upstream merge: the only recorded event a merge-order
	// edge resolves on, never an agent outcome (spec#11).
	pull.Mu.Lock()
	pull.Prs[0].Merged = true
	pull.Prs[0].MergeCommit = "aabbccddeeff0011223344556677889900aabbccdd"
	pull.Mu.Unlock()
	// edgeResolverPass polls no more often than once per configured interval; advancing
	// the fixture's clock well past that keeps this second call from being
	// skipped as a too-recent repeat of the first.
	f.clock.Mu.Lock()
	f.clock.At = f.clock.At.Add(11 * time.Minute)
	f.clock.Mu.Unlock()
	must(t, f.s.edgeResolverPass(ctx, repository))
	if pending, err := upstreamDeliveryPending(repository, f.s.about(repository), stream); err != nil || !pending {
		t.Fatalf("the upstream merge was not recognised as pending maintenance: %v %v", pending, err)
	}

	a := &finalReviewer{s: f.s, repository: repository}
	must(t, recordOwnPullRequestOutcome(ctx, repo2, f.s.now(), upstream, "fixture", pull.Prs[0].Number, ownPullRequestOutcomeMerged))
	if done, err := resolved(repo2, upstream); err != nil || !done {
		t.Fatalf("merged upstream is not resolved: %v, %v", done, err)
	}
	assertContext(true)
	if done, err := resolved(repository, stream); err != nil || done {
		t.Fatalf("dependent resolved before its update and approval: %v, %v", done, err)
	}
	updates, err := followup.Read(repository, stream)
	must(t, err)
	if len(updates) != 1 || !strings.Contains(updates[0].Unit.Task, pull.Prs[0].MergeCommit) {
		t.Fatalf("merged dependency update units: %+v", updates)
	}
	must(t, f.s.edgeResolverPass(ctx, repository))
	retried, err := followup.Read(repository, stream)
	must(t, err)
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
	runReview := func(gap bool) FinalReport {
		t.Helper()
		f.clock.Mu.Lock()
		f.clock.At = f.clock.At.Add(time.Second)
		f.clock.Mu.Unlock()
		must(t, a.Pass(ctx))
		op := actionOperation(t, repository, stream, deliveryReviewAction)
		in, err := decodeFinalReview(op)
		must(t, err)
		f.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
			findings := []any{}
			if gap {
				findings = append(findings, map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "TestDedupe fails with the merged dependency; repair dedupe.go within the slice"})
			}
			_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Dependency updated", "findings": findings})
			return err
		})
		result := settleOperation(t, f.s, repository, stream, op, a)
		if result.Outcome != "succeeded" {
			t.Fatal(result)
		}
		fresh, found, err := latestFinalReport(repository, stream)
		must(t, err)
		if !found || fresh.EdgeResolved == "" || fresh.Commit == report.Commit {
			t.Fatalf("fresh review did not read the dependency update: %+v", fresh)
		}
		_, checks, err := a.incompleteChecks(stream, in.Review, fresh.Commit)
		must(t, err)
		if checks.Commit != fresh.Commit {
			t.Fatalf("checks did not read the update: %+v", checks)
		}
		assertContext(true)
		return fresh
	}
	fresh := runReview(repair)
	if repair {
		units, err := followup.Read(repository, stream)
		must(t, err)
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
	wantChecks := 1
	if repair {
		wantChecks = 2
	}
	if checkCalls != wantChecks {
		t.Fatalf("checks ran %d times, want %d", checkCalls, wantChecks)
	}

	// Only a fresh delivery approval (the second and only other owner
	// prompt) authorises marking the existing pull request ready: never
	// before it (spec#13, spec#14).
	if pull.Readies != 0 {
		t.Fatal("the dependent's pull request was marked ready before any fresh delivery approval")
	}
	freshApproval := pub.approve(t, nil)
	if freshApproval.Review != fresh.Review {
		t.Fatalf("the fresh approval read review %d, want %d", freshApproval.Review, fresh.Review)
	}
	// The dependent is already delivered, so this fresh republish reuses the
	// existing delivery-maintenance action, exactly as a same-repo base
	// dependency's post-integration republish does (base_delivery_test.go).
	must(t, pub.publisher().Pass(ctx))
	op2 := actionOperation(t, repository, stream, publishUpstreamAction)
	result2, err := pub.publisher().Apply(ctx, op2)
	if err != nil || result2.Outcome != "succeeded" {
		t.Fatalf("the fresh publication %+v: %v", result2, err)
	}
	publication, found, err := currentOwnPublication(repository, stream)
	must(t, err)
	if !found || publication.Approval != 2 || publication.Commit == firstPublication.Commit || !strings.Contains(demoGit(t, home, "-C", f.clone, "cat-file", "-p", publication.Commit), "gpgsig") {
		t.Fatalf("ready publication lacks fresh owner approval and signing: %+v", publication)
	}
	if got := strings.TrimSpace(demoGit(t, home, "-C", f.clone, "show", publication.Commit+":dependency.txt")); got != pull.Prs[0].MergeCommit {
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

// seedEdgeDependentBase adds an unintegrated, delivered same-repo base
// to the fixture before either publication of the workgroup dependent.
func seedEdgeDependentBase(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, report FinalReport) (config.WorkstreamID, FinalReport) {
	t.Helper()
	ctx := context.Background()
	parent := config.WorkstreamID("w_fffffffffffffffffffffffffffffff0")
	must(t, repo.CreateWorkstream(ctx, parent, f.s.now(), ownerActor))
	sealed, doc, _, err := seal.Latest(repo, stream)
	must(t, err)
	demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "branch", featureBranch(parent), sealed.Base.Commit)
	parentTip := moveFeature(t, f, parent, map[string]string{"BASE.md": "same-repo dependency\n"})
	demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "push", "--quiet", "origin", parentTip+":refs/heads/"+featureBranch(parent))
	parentSeal := sealed
	parentSeal.Branch = featureBranch(parent)
	data, err := seal.Encode(parentSeal)
	must(t, err)
	parentDoc := doc
	parentDoc.Workstream, parentDoc.Content, parentDoc.Revision = parent, string(data), 1
	must(t, repo.RecordDocuments(ctx, []trace.Document{parentDoc}))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "parent-delivered", Revision: 1, Project: repo.Project(), Workstream: parent, At: f.s.now(), Actor: foremanActor, Cause: "fixture"}
	_, err = repo.SetFeatureState(ctx, h, DeliveredState, "Same-repo base delivered before integration")
	must(t, err)
	base := trace.WorkstreamBase{Workstream: stream, Base: parent, Revision: 1}
	data, err = json.Marshal(base)
	must(t, err)
	baseDoc := doc
	baseDoc.ID, baseDoc.Path, baseDoc.Content, baseDoc.Revision = "workstream-base", "base.json", string(data), 1
	must(t, repo.RecordDocuments(ctx, []trace.Document{baseDoc}))

	g, err := featureWorkspaces(f.s.about(repo), repo).Of(stream)
	must(t, err)
	w, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Branch: featureBranch(stream)})
	must(t, err)
	tip, conflicts, err := g.ReplayFrom(ctx, sealed.Base.Commit, report.Commit, parentTip, f.s.now())
	must(t, err)
	if len(conflicts) != 0 {
		t.Fatalf("seeding base conflicts: %v", conflicts)
	}
	must(t, g.Move(ctx, w.(workspace.Worktree), report.Commit, tip))
	sealed.Base = seal.Base{Remote: ".", Branch: featureBranch(parent), Commit: parentTip, Workstream: parent}
	data, err = seal.Encode(sealed)
	must(t, err)
	doc.Revision++
	doc.Content = string(data)
	must(t, repo.RecordDocuments(ctx, []trace.Document{doc}))
	report.Commit, report.Upstream = tip, &sealed.Base
	data, err = json.Marshal(report)
	must(t, err)
	h.Schema, h.ID, h.Workstream, h.Revision = "osmia.trace.document", finalReportDocument, stream, 2
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: finalReportPath, Content: string(data)}}))
	return parent, report
}
func exerciseReadyDependentBaseGap(t *testing.T, p *publicationFixture, parent config.WorkstreamID) {
	t.Helper()
	ctx := context.Background()
	repo, stream := p.repository, p.stream
	if pending, err := workgroupEdgePending(repo, stream); err != nil || pending {
		t.Fatalf("ready publication retains edge repair permission: %v, %v", pending, err)
	}
	if resolved, err := workgroupMergeOrderResolved(repo, stream); err != nil || resolved == "" {
		t.Fatalf("ready publication lost historical edge resolution: %q, %v", resolved, err)
	}
	before, err := followup.Read(repo, stream)
	must(t, err)
	upstreamTip := advanceUpstream(t, p.shedFixture, map[string]string{"BASE.md": "same-repo dependency\n", "INTEGRATED.md": "base is integrated\n"})
	parentTip := strings.TrimSpace(demoGit(t, filepath.Dir(p.clone), "-C", p.clone, "rev-parse", featureBranch(parent)))
	merged := DeliveryMerge{Branch: featureBranch(parent), Commit: parentTip, Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: upstreamTip}}
	data, err := json.Marshal(merged)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: mergeDocument, Revision: 1, Project: repo.Project(), Workstream: parent, At: p.s.now(), Actor: ownerActor, Cause: "fixture"}
	must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: mergePath, Content: string(data)}}))
	jump(p.clock, p.s.now().Add(11*time.Minute))
	refresh := &baseRefresher{s: p.s, repository: repo}
	must(t, refresh.Pass(ctx))
	op := actionOperation(t, repo, stream, baseRefreshAction)
	if result := settleOperation(t, p.s, repo, stream, op, refresh); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	if pending, err := upstreamDeliveryPending(repo, p.s.about(repo), stream); err != nil || !pending {
		t.Fatalf("later base integration did not schedule maintenance: %v, %v", pending, err)
	}
	reviewer := &finalReviewer{s: p.s, repository: repo}
	must(t, reviewer.Pass(ctx))
	op = actionOperation(t, repo, stream, deliveryReviewAction)
	in, err := decodeFinalReview(op)
	must(t, err)
	p.finalTurn(in.Review, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Base integration exposes a gap", "findings": []any{map[string]any{"severity": "blocking", "criteria": []any{"spec#1"}, "finding": "resume behavior is missing after base integration"}}})
		return err
	})
	if result := settleOperation(t, p.s, repo, stream, op, reviewer); result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	report, reason, err := reviewer.finalGate(ctx, stream)
	must(t, err)
	if !report.Maintenance || report.EdgeResolved == "" || reason != "" || len(reportBlocking(report)) != 1 || p.feature(t) != DeliveredState {
		t.Fatalf("later maintenance gap: %+v reason %q state %s", report, reason, p.feature(t))
	}
	if body := noticeOf(t, repo, stream, fmt.Sprintf("final-review-%d-reviewed", in.Review)); !strings.Contains(body, "move the workstream to building with move_workstream") {
		t.Fatalf("the chief heard %q", body)
	}
	after, err := followup.Read(repo, stream)
	must(t, err)
	if len(after) != len(before) {
		t.Fatalf("later maintenance created in-place repairs: before %+v, after %+v", before, after)
	}
	build := &masons{s: p.s, cfg: p.s.about(repo), repository: repo}
	if _, admitted, err := build.read(stream); err != nil || admitted {
		t.Fatalf("ready workgroup dependent admitted implementation: %v, %v", admitted, err)
	}
	land := &foreman{masons: build}
	if reason, err := land.current(ctx, stream, landInput{}, UnitReviewResult{}); err != nil || !strings.Contains(reason, "not building or assembled") {
		t.Fatalf("later maintenance admitted landing: %q, %v", reason, err)
	}
	presented, api := p.s.deliveryPresentation(ctx, string(stream))
	if api != nil {
		t.Fatal(api)
	}
	decision := DeliveryDecision{Review: report.Review, ReviewRevision: presented.ReviewRevision, Commit: report.Commit, DraftHash: presented.DraftHash}
	if _, api := p.s.approveDelivery(ctx, string(stream), decision); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "approve with accept_blocking") {
		t.Fatalf("later maintenance gap accepted delivery approval: %v", api)
	}
	must(t, p.publisher().Pass(ctx))
	latest := actionOperation(t, repo, stream, publishUpstreamAction)
	publication, found, err := currentOwnPublication(repo, stream)
	must(t, err)
	if !found || latest.ID != publication.Operation || publication.Approval != 2 || publication.Draft {
		t.Fatalf("later maintenance authorized publication: %+v, %+v", latest, publication)
	}
}

// TestWorkgroupDependentPostMergeGapRepairsInScopeNeverBlocksLikeSameRepoMaintenance
// covers the other half of spec#13: when the post-merge update makes a
// check or review finding fail, the dependent repairs it within its own
// slice through an ordinary follow-up unit, never the same-repo rule that
// base-dependency delivery maintenance blocks outright on any gap (design
// §7.6), and never a new workstream or an added owner prompt.
func TestWorkgroupDependentPostMergeGapRepairsInScopeNeverBlocksLikeSameRepoMaintenance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository, report, repo2, upstream := newCrossRepoEdgeDependent(t)
	a := &finalReviewer{s: f.s, repository: repository}

	// The upstream's merge, recorded the other way an edge may resolve: the
	// owner's own record of a merge done outside the factory (osmia
	// merged), rather than a detected pull request.
	_, err := repo2.SetFeatureState(ctx, trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "deliver", Revision: 1, Project: repo2.Project(), Workstream: upstream, At: f.s.now(), Actor: ownerActor, Cause: "fixture"}, DeliveredState, "fixture delivery")
	must(t, err)
	mergeRecord := DeliveryMerge{Branch: featureBranch(upstream), Commit: "e1f2a3b4c5d6e1f2a3b4c5d6e1f2a3b4c5d6e1f2", Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: "e1f2a3b4c5d6e1f2a3b4c5d6e1f2a3b4c5d6e1f2"}}
	mergeData, err := json.Marshal(mergeRecord)
	must(t, err)
	must(t, repo2.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: mergeDocument, Revision: 1, Project: repo2.Project(), Workstream: upstream, At: f.s.now(), Actor: ownerActor, Cause: "fixture"}, Path: mergePath, Content: string(mergeData)}}))
	must(t, f.s.edgeResolverPass(ctx, repository))

	in, sealed, err := a.governing(ctx, stream)
	must(t, err)
	if in.EdgeResolved == "" {
		t.Fatalf("governing inputs do not yet show the upstream merge resolved: %+v", in)
	}

	// The update's fresh final review finds spec#2 broken: a check or
	// review failure the update itself caused.
	gapped := FinalReport{Review: report.Review + 1, Outcome: finalReviewed, Branch: featureBranch(stream), Commit: in.Commit, Seal: in.Seal, SpecHash: sealed.SpecHash, Spec: in.Spec, Plan: in.Plan, Charter: in.Charter, Upstream: &sealed.Base, Maintenance: in.Maintenance, EdgeResolved: in.EdgeResolved, Summary: report.Summary, Criteria: []FinalCriterion{
		{Criterion: "spec#1", Evidence: "resume.go and TestResume"},
		{Criterion: "spec#2", Gap: "TestDedupe now fails after updating the dependency reference"},
	}}
	data, err := json.Marshal(gapped)
	must(t, err)
	must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalReportDocument, Revision: 2, Project: repository.Project(), Workstream: stream, At: f.s.now(), Actor: finalReviewActor, Cause: "final-review"}, Path: finalReportPath, Content: string(data)}}))

	// The gap becomes an ordinary follow-up unit of the same workstream,
	// never a new workstream, and recording it raises nothing to the
	// owner: no new inbox entry.
	before, err := repository.Inbox()
	must(t, err)
	added, found, err := a.followup(stream, gapped, 2)
	must(t, err)
	if !found || added.Criterion != "spec#2" || added.Gap != "spec#2: "+gapped.Criteria[1].Gap || added.Unit.ID == "" {
		t.Fatalf("follow-up unit for the post-merge gap: %+v", added)
	}
	after, err := repository.Inbox()
	must(t, err)
	if len(after) != len(before) {
		t.Fatalf("computing the in-scope repair's follow-up unit added %d inbox entries", len(after)-len(before))
	}
}
