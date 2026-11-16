package lifecycle_test

import (
	context "context"
	json "encoding/json"
	bundle "github.com/kpenfound/osmia/internal/bundle"
	pulls "github.com/kpenfound/osmia/internal/pulls"
	os "os"
	filepath "path/filepath"
	strings "strings"
	testing "testing"
	time "time"
)

func TestMergeOrderDependentBuildReferencesAndLinkedDraftPublication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository, report, upstreamRepo, upstream := newCrossRepoEdgeDependent(t)
	must(t, f.s.edgeResolverPass(ctx, repository))
	provider := f.s.Context()
	assertBuildReference := func(want, reference string) {
		t.Helper()
		view, err := provider.Assemble(ctx, repository.Project(), bundle.Scope{Role: masonRole, Workstream: stream})
		must(t, err)
		if len(view.Dependencies) != 1 {
			t.Fatalf("dependent context: %+v", view.Dependencies)
		}
		dependency := view.Dependencies[0]
		if dependency.Upstream != upstream || dependency.Merged || dependency.BuildBranch != want || dependency.Reference != reference {
			t.Fatalf("dependent build reference: %+v; want %q and %q, unmerged", dependency, want, reference)
		}
		if rendered := view.Render(); !strings.Contains(rendered, "build reference: "+want) {
			t.Fatalf("role context omits upstream build identity: %s", rendered)
		}
	}
	assertBuildReference("owner/other-fork@"+featureBranch(upstream), "")
	if _, api := f.s.deliveryPresentation(ctx, string(stream)); api == nil || api.Code != Conflict || !strings.Contains(api.Message, "waiting on owner:") {
		t.Fatalf("publication without an upstream PR reference must wait: %v", api)
	}

	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	configureDeliverySigner(t, f.clone)
	must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	demoGit(t, home, "init", "--quiet", "--bare", fork)
	demoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	pull := &fakePulls{SyncHead: true, Fork: func() string {
		return strings.TrimSpace(demoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+featureBranch(stream)))
	}}
	f.s.mu.Lock()
	f.s.options.PullRequests = pull
	f.s.mu.Unlock()
	publication := DeliveryPublication{Status: publicationOpened, Upstream: "owner/other", Fork: "owner/other-fork", Branch: featureBranch(upstream), Commit: "d1e2f3a4b5c6d1e2f3a4b5c6d1e2f3a4b5c6d1e2", PullRequest: 77, URL: "https://example.invalid/owner/other/pull/77"}
	recordUpstreamOwnPullRequest(t, f.s, upstreamRepo, upstream, publication)
	pull.Prs = append(pull.Prs, pulls.PullRequest{Number: publication.PullRequest, URL: publication.URL, State: "open", HeadRepository: publication.Fork, Head: publication.Branch, HeadCommit: publication.Commit, Base: "main"})
	f.clock.Mu.Lock()
	f.clock.At = f.clock.At.Add(11 * time.Minute)
	f.clock.Mu.Unlock()
	must(t, f.s.edgeResolverPass(ctx, repository))
	assertBuildReference(publication.Fork+"@"+publication.Commit, "owner/other#77")

	pub := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: report, fork: fork, pulls: pull}
	must(t, pub.publisher().Pass(ctx))
	if operations := pub.operations(t); len(operations) != 0 {
		t.Fatalf("unapproved dependent publication scheduled: %+v", operations)
	}
	presented, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil {
		t.Fatal(api)
	}
	if !presented.DraftPullRequest || !strings.Contains(presented.Draft, "stays a draft") {
		t.Fatalf("a pending merge-order dependency must present a draft pull request that the description states: %v, %s", presented.DraftPullRequest, presented.Draft)
	}
	if encoded, err := json.Marshal(presented); err != nil || !strings.Contains(string(encoded), `"draft_pull_request":true`) {
		t.Fatalf("presentation omits the draft pull request signal: %s, %v", encoded, err)
	}
	missingDependency := "Implement the dependent interface."
	_, api = f.s.approveDelivery(ctx, string(stream), DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash, Description: &missingDependency})
	if api == nil || api.Code != Validation || !strings.Contains(api.Message, "description must state the dependency on owner/other#77") {
		t.Fatalf("approval accepted an omitted upstream reference: %v", api)
	}
	approval := pub.approve(t, nil)
	for _, text := range []string{"depends on owner/other#77", "must merge first", "stays a draft"} {
		if !strings.Contains(approval.Description, text) {
			t.Fatalf("approved description omits %q: %s", text, approval.Description)
		}
	}
	op := pub.request(t)
	result, err := pub.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("draft publication: %+v, %v", result, err)
	}
	// The approval is recorded and applied, and the pull request stays a draft
	// until the dependency has merged.
	if got := pub.feature(t); got != DeliveredState {
		t.Fatalf("the dependent is %s after its approval", got)
	}
	if state, err := repository.Workflow(stream, "publication"); err != nil || state.Value != "published-1" {
		t.Fatalf("the approval's outcome %+v: %v", state, err)
	}
	must(t, pub.publisher().Pass(ctx))
	pull.Mu.Lock()
	defer pull.Mu.Unlock()
	if len(pull.Prs) != 2 || pull.Creates != 1 || pull.Readies != 0 {
		t.Fatalf("dependent publication: %+v; creates %d, ready calls %d", pull.Prs, pull.Creates, pull.Readies)
	}
	if pr := pull.Prs[1]; !pr.Draft || !strings.Contains(pr.Body, "owner/other#77") {
		t.Fatalf("the pull request must be a draft that links the dependency: %+v", pr)
	}
}

func TestDeliveryPresentationOmitsDraftPullRequestWithoutPendingDependency(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "squash")
	presented, api := p.s.deliveryPresentation(context.Background(), string(p.stream))
	if api != nil {
		t.Fatal(api)
	}
	if presented.DraftPullRequest || strings.Contains(presented.Draft, "stays a draft") {
		t.Fatalf("a workstream without a pending dependency presents a draft pull request: %v, %s", presented.DraftPullRequest, presented.Draft)
	}
	encoded, err := json.Marshal(presented)
	must(t, err)
	var fields map[string]json.RawMessage
	must(t, json.Unmarshal(encoded, &fields))
	if _, present := fields["draft_pull_request"]; present {
		t.Fatalf("presentation serializes draft_pull_request without a pending dependency: %s", encoded)
	}
}
