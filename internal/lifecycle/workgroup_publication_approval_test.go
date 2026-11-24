package lifecycle_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestWorkgroupPublicationRequiresItsOwnDeliveryApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository, report, upstreamRepository, upstream := newCrossRepoEdgeDependent(t)
	upstreamPublication := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "owner/other", Fork: "owner/other-fork", Branch: intake.FeatureBranch(upstream), Commit: "d1e2f3a4b5c6d1e2f3a4b5c6d1e2f3a4b5c6d1e2", PullRequest: 77, URL: "https://example.invalid/owner/other/pull/77"}
	journey.RecordUpstreamOwnPullRequest(t, f.S, upstreamRepository, upstream, upstreamPublication)
	client := &fixturekit.FakePulls{Prs: []pulls.PullRequest{{Number: upstreamPublication.PullRequest, URL: upstreamPublication.URL, State: "open", HeadRepository: upstreamPublication.Fork, Head: upstreamPublication.Branch, HeadCommit: upstreamPublication.Commit, Base: "main"}}}
	f.S.Options.PullRequests = client
	fixturekit.Must(t, f.S.Lifecycle().EdgeResolverPass(ctx, repository))
	p := &journey.PublicationFixture{ShedFixture: f, Stream: stream, Repository: repository, Report: report, Pulls: client}

	if _, reason, err := f.S.DeliveryGate(ctx, repository, stream, "Depends on owner/other#77."); err != nil || reason != "the owner has not marked the pull request ready for review" {
		t.Fatalf("group ratification delivery gate: %q, %v", reason, err)
	}
	fixturekit.Must(t, p.Publisher().Pass(ctx))
	if ops := p.Operations(t); len(ops) != 0 {
		t.Fatalf("group ratification requested publication without this member's delivery approval: %+v", ops)
	}

	approval := p.Approve(t, nil)
	op := p.Request(t)
	in, err := delivery.DecodePublish(op)
	fixturekit.Must(t, err)
	if in.Approval != 1 || in.Commit != approval.Commit || in.DescriptionHash != approval.DescriptionHash || !in.Draft {
		t.Fatalf("publication did not pin this member's approved draft: %+v", in)
	}
	_, _, siblingApproval, err := delivery.DeliveryDocuments(upstreamRepository, upstream)
	fixturekit.Must(t, err)
	if siblingApproval != nil {
		t.Fatal("approving one member also approved its sibling")
	}

	finds := client.Finds
	result, err := (&journey.Publisher{S: f.S, Repository: upstreamRepository}).Apply(ctx, op)
	if err == nil || !strings.Contains(err.Error(), "belongs to no workstream") || result.Outcome == "succeeded" {
		t.Fatalf("another member accepted the approved publication: %+v, %v", result, err)
	}
	if client.Finds != finds || client.Creates != 0 || client.Updates != 0 || client.Readies != 0 {
		t.Fatalf("refused sibling publication reached the pull request host: %+v", client)
	}
}
