package lifecycle_test

import (
	context "context"
	json "encoding/json"
	os "os"
	filepath "path/filepath"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	bundle "github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	pulls "github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestMergeOrderDependentBuildReferencesAndLinkedDraftPublication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository, report, upstreamRepo, upstream := newCrossRepoEdgeDependent(t)
	fixturekit.Must(t, f.S.Lifecycle().EdgeResolverPass(ctx, repository))
	provider := f.S.Context()
	assertBuildReference := func(want, reference string) {
		t.Helper()
		view, err := provider.Assemble(ctx, repository.Project(), bundle.Scope{Role: intake.MasonRole, Workstream: stream})
		fixturekit.Must(t, err)
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
	assertBuildReference("owner/other-fork@"+intake.FeatureBranch(upstream), "")
	if _, apiFault := f.S.DeliveryPresentation(ctx, string(stream)); apiFault == nil || apiFault.Code != api.Conflict || !strings.Contains(apiFault.Message, "waiting on owner:") {
		t.Fatalf("publication without an upstream PR reference must wait: %v", apiFault)
	}

	home := filepath.Dir(f.Clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	fixturekit.ConfigureDeliverySigner(t, f.Clone)
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	fixturekit.DemoGit(t, home, "init", "--quiet", "--bare", fork)
	fixturekit.DemoGit(t, home, "-C", f.Clone, "remote", "add", "origin", fork)
	pull := &fixturekit.FakePulls{SyncHead: true, Fork: func() string {
		return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+intake.FeatureBranch(stream)))
	}}
	f.S.Mu.Lock()
	f.S.Options.PullRequests = pull
	f.S.Mu.Unlock()
	publication := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "owner/other", Fork: "owner/other-fork", Branch: intake.FeatureBranch(upstream), Commit: "d1e2f3a4b5c6d1e2f3a4b5c6d1e2f3a4b5c6d1e2", PullRequest: 77, URL: "https://example.invalid/owner/other/pull/77"}
	journey.RecordUpstreamOwnPullRequest(t, f.S, upstreamRepo, upstream, publication)
	pull.Prs = append(pull.Prs, pulls.PullRequest{Number: publication.PullRequest, URL: publication.URL, State: "open", HeadRepository: publication.Fork, Head: publication.Branch, HeadCommit: publication.Commit, Base: "main"})
	f.Clock.Mu.Lock()
	f.Clock.At = f.Clock.At.Add(11 * time.Minute)
	f.Clock.Mu.Unlock()
	fixturekit.Must(t, f.S.Lifecycle().EdgeResolverPass(ctx, repository))
	assertBuildReference(publication.Fork+"@"+publication.Commit, "owner/other#77")

	pub := &journey.PublicationFixture{ShedFixture: f, Stream: stream, Repository: repository, Report: report, Fork: fork, Pulls: pull}
	fixturekit.Must(t, pub.Publisher().Pass(ctx))
	if operations := pub.Operations(t); len(operations) != 0 {
		t.Fatalf("unapproved dependent publication scheduled: %+v", operations)
	}
	presented, apiFault := f.S.DeliveryPresentation(ctx, string(stream))
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	if !presented.DraftPullRequest || !strings.Contains(presented.Draft, "stays a draft") {
		t.Fatalf("a pending merge-order dependency must present a draft pull request that the description states: %v, %s", presented.DraftPullRequest, presented.Draft)
	}
	if encoded, err := json.Marshal(presented); err != nil || !strings.Contains(string(encoded), `"draft_pull_request":true`) {
		t.Fatalf("presentation omits the draft pull request signal: %s, %v", encoded, err)
	}
	missingDependency := "Implement the dependent interface."
	_, apiFault = f.S.ApproveDelivery(ctx, string(stream), api.DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash, Description: &missingDependency})
	if apiFault == nil || apiFault.Code != api.Validation || !strings.Contains(apiFault.Message, "description must state the dependency on owner/other#77") {
		t.Fatalf("approval accepted an omitted upstream reference: %v", apiFault)
	}
	pub.PublishDraft(t)
	approval := pub.Approve(t, nil)
	for _, text := range []string{"depends on owner/other#77", "must merge first", "stays a draft"} {
		if !strings.Contains(approval.Description, text) {
			t.Fatalf("approved description omits %q: %s", text, approval.Description)
		}
	}
	op := pub.Request(t)
	result, err := pub.Publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("draft publication: %+v, %v", result, err)
	}
	if got := pub.Feature(t); got != intake.DeliveredState {
		t.Fatalf("the dependent is %s after its approval", got)
	}
	if state, err := repository.Workflow(stream, "publication"); err != nil || state.Value != "published-1" {
		t.Fatalf("the approval's outcome %+v: %v", state, err)
	}
	fixturekit.Must(t, pub.Publisher().Pass(ctx))
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
	p := journey.NewPublicationFixture(t, "squash")
	presented, apiFault := p.S.DeliveryPresentation(context.Background(), string(p.Stream))
	if apiFault != nil {
		t.Fatal(apiFault)
	}
	if presented.DraftPullRequest || strings.Contains(presented.Draft, "stays a draft") {
		t.Fatalf("a workstream without a pending dependency presents a draft pull request: %v, %s", presented.DraftPullRequest, presented.Draft)
	}
	encoded, err := json.Marshal(presented)
	fixturekit.Must(t, err)
	var fields map[string]json.RawMessage
	fixturekit.Must(t, json.Unmarshal(encoded, &fields))
	if _, present := fields["draft_pull_request"]; present {
		t.Fatalf("presentation serializes draft_pull_request without a pending dependency: %s", encoded)
	}
}
