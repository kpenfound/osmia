package executionjourney

import (
	"context"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestDeliveryPresentationShowsTheTitleAndKeepsTheBodyEditable(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := deliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	presented, apiErr := f.s.deliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if presented.Title != "Resumable uploads" {
		t.Fatalf("presentation title %q", presented.Title)
	}
	if len(presented.Report.Criteria) != 2 || presented.Report.Criteria[0].Evidence == "" || presented.Report.Criteria[1].Evidence == "" {
		t.Fatalf("presentation report %+v does not carry every criterion's evidence", presented.Report)
	}
	edited := "Owner's own words about the change.\n"
	approval, apiErr := f.s.approveDelivery(ctx, string(stream), api.DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash, Description: &edited})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if approval.Description != edited || approval.Title != presented.Title {
		t.Fatalf("approval %+v, want the edited body and the presented title", approval)
	}
}

func TestDeliveryApprovalIsStaleWhenTheStatusGoalChangesBeforeApproval(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := deliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	first, apiErr := f.s.deliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	fixturekit.SetStatusGoal(t, repository, stream, "A new status goal", time.Now().UTC())
	if _, apiErr := f.s.approveDelivery(ctx, string(stream), api.DeliveryDecision{Review: first.Report.Review, ReviewRevision: first.ReviewRevision, Commit: first.Report.Commit, DraftHash: first.DraftHash}); apiErr == nil || apiErr.Code != api.Conflict {
		t.Fatalf("approved a presentation whose title the owner was not shown: %v", apiErr)
	}
	second, apiErr := f.s.deliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if second.Title != "A new status goal" || second.DraftHash == first.DraftHash {
		t.Fatalf("fresh presentation %+v does not show the new title", second)
	}
	approval, apiErr := f.s.approveDelivery(ctx, string(stream), api.DeliveryDecision{Review: second.Report.Review, ReviewRevision: second.ReviewRevision, Commit: second.Report.Commit, DraftHash: second.DraftHash})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if approval.Title != "A new status goal" {
		t.Fatalf("approval %+v did not record the new title", approval)
	}
}

func TestPublicationOpensThePullRequestWithTheStatusGoalAsItsTitle(t *testing.T) {
	t.Parallel()
	p := newUnpushedPublicationFixture(t, "commit-per-unit", "")
	ctx := context.Background()
	long := strings.Repeat("Ship the resumable upload feature to every customer ", 3)
	fixturekit.SetStatusGoal(t, p.repository, p.stream, long, p.s.now())
	p.publishDraft(t)
	p.approve(t, nil)
	op := p.request(t)
	result, err := p.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	want := string([]rune(long)[:execution.SubjectLength-1]) + "…"
	if runes := []rune(want); len(runes) != execution.SubjectLength || len(runes) >= len([]rune(long)) {
		t.Fatalf("fixture goal %q was not long enough to exercise the bound", long)
	}
	if len(p.pulls.Prs) != 1 || p.pulls.Prs[0].Title != want {
		t.Fatalf("pull request title %q, want the bounded status goal %q, not the final review summary %q", p.pulls.Prs[0].Title, want, p.report.Summary)
	}
}

func TestPublicationUsesTheApprovedTitleDespiteALaterStatusGoalChange(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	approval := p.approve(t, nil)
	if approval.Title != "Resumable uploads" {
		t.Fatalf("approval title %q", approval.Title)
	}
	fixturekit.SetStatusGoal(t, p.repository, p.stream, "A completely different later goal", p.s.now())
	op := p.request(t)
	result, err := p.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	if len(p.pulls.Prs) != 1 || p.pulls.Prs[0].Title != "Resumable uploads" {
		t.Fatalf("pull request %+v, want the approved title unaffected by the later status goal", p.pulls.Prs)
	}
}

func TestSameRepositoryMaintenanceUpdatesAnExistingPullRequestsTitle(t *testing.T) {
	t.Parallel()
	p := sameRepositoryMaintenance(t)
	before := p.pulls.Prs[0].Title
	fixturekit.SetStatusGoal(t, p.repository, p.stream, "A refreshed delivery title", p.s.now())
	approveMaintenance(t, p)
	if before == "A refreshed delivery title" {
		t.Fatal("fixture already used the new title")
	}
	if p.pulls.Prs[0].Title != before {
		t.Fatalf("the live pull request's title moved before publication: %q", p.pulls.Prs[0].Title)
	}
	op := fixturekit.ActionOperation(t, p.repository, p.stream, publishUpstreamAction)
	in, err := delivery.DecodePublish(op)
	fixturekit.Must(t, err)
	if in.Retarget == nil || in.Retarget.Title != before {
		t.Fatalf("retarget %+v, want the prior title %q", in.Retarget, before)
	}
	result, err := p.publisher().Apply(context.Background(), op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" || p.pulls.Updates != 1 {
		t.Fatalf("publication %+v, updates %d", result, p.pulls.Updates)
	}
	if p.pulls.Prs[0].Title != "A refreshed delivery title" {
		t.Fatalf("pull request title %q, want it updated to the approved title", p.pulls.Prs[0].Title)
	}
}

func TestPresentingTheSameTraceTwiceIsDeterministicAndCallsNoAgent(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := deliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	first, apiErr := f.s.deliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	second, apiErr := f.s.deliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if first.Title != second.Title || first.Draft != second.Draft || first.DraftHash != second.DraftHash {
		t.Fatalf("two presentations of the same trace differ: %+v, %+v", first, second)
	}
}
