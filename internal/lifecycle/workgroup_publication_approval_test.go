package lifecycle_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func TestWorkgroupPublicationRequiresItsOwnDeliveryApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository, report, upstreamRepository, upstream := newCrossRepoEdgeDependent(t)
	upstreamPublication := api.DeliveryPublication{Status: delivery.PublicationOpened, Upstream: "owner/other", Fork: "owner/other-fork", Branch: intake.FeatureBranch(upstream), Commit: "d1e2f3a4b5c6d1e2f3a4b5c6d1e2f3a4b5c6d1e2", PullRequest: 77, URL: "https://example.invalid/owner/other/pull/77"}
	recordUpstreamOwnPullRequest(t, f.s, upstreamRepository, upstream, upstreamPublication)
	client := &fixturekit.FakePulls{Prs: []pulls.PullRequest{{Number: upstreamPublication.PullRequest, URL: upstreamPublication.URL, State: "open", HeadRepository: upstreamPublication.Fork, Head: upstreamPublication.Branch, HeadCommit: upstreamPublication.Commit, Base: "main"}}}
	f.s.options.PullRequests = client
	intakefixture.Must(t, f.s.edgeResolverPass(ctx, repository))
	p := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: report, pulls: client}

	if _, reason, err := f.s.deliveryGate(ctx, repository, stream, "Depends on owner/other#77."); err != nil || reason != "the owner has not marked the pull request ready for review" {
		t.Fatalf("group ratification delivery gate: %q, %v", reason, err)
	}
	intakefixture.Must(t, p.publisher().Pass(ctx))
	if ops := p.operations(t); len(ops) != 0 {
		t.Fatalf("group ratification requested publication without this member's delivery approval: %+v", ops)
	}

	approval := p.approve(t, nil)
	op := p.request(t)
	in, err := delivery.DecodePublish(op)
	intakefixture.Must(t, err)
	if in.Approval != 1 || in.Commit != approval.Commit || in.DescriptionHash != approval.DescriptionHash || !in.Draft {
		t.Fatalf("publication did not pin this member's approved draft: %+v", in)
	}
	_, _, siblingApproval, err := delivery.DeliveryDocuments(upstreamRepository, upstream)
	intakefixture.Must(t, err)
	if siblingApproval != nil {
		t.Fatal("approving one member also approved its sibling")
	}

	finds := client.Finds
	result, err := (&publisher{s: f.s, repository: upstreamRepository}).Apply(ctx, op)
	if err == nil || !strings.Contains(err.Error(), "belongs to no workstream") || result.Outcome == "succeeded" {
		t.Fatalf("another member accepted the approved publication: %+v, %v", result, err)
	}
	if client.Finds != finds || client.Creates != 0 || client.Updates != 0 || client.Readies != 0 {
		t.Fatalf("refused sibling publication reached the pull request host: %+v", client)
	}
}
