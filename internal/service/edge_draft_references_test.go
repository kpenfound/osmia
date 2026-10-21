package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/pulls"
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
	missingDependency := "Implement the dependent interface."
	_, api = f.s.approveDelivery(ctx, string(stream), DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash, Description: &missingDependency})
	if api == nil || api.Code != Validation || !strings.Contains(api.Message, "description must state the dependency on owner/other#77") {
		t.Fatalf("approval accepted an omitted upstream reference: %v", api)
	}
	approval := pub.approve(t, nil)
	for _, text := range []string{"depends on owner/other#77", "must merge first", "opens as a draft"} {
		if !strings.Contains(approval.Description, text) {
			t.Fatalf("approved description omits %q: %s", text, approval.Description)
		}
	}
	op := pub.request(t)
	result, err := pub.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("draft publication: %+v, %v", result, err)
	}
	pull.Mu.Lock()
	defer pull.Mu.Unlock()
	if len(pull.Prs) != 2 || pull.Creates != 1 || pull.Readies != 0 {
		t.Fatalf("dependent publication: %+v; creates %d, ready calls %d", pull.Prs, pull.Creates, pull.Readies)
	}
	if pr := pull.Prs[1]; !pr.Draft || pr.Body != approval.Description {
		t.Fatalf("published PR must be a draft with the approved dependency text: %+v", pr)
	}
}
