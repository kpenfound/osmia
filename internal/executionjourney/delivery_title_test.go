package executionjourney

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestDeliveryPresentationShowsTheTitleAndKeepsTheBodyEditable(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.DeliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	presented, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
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
	approval, apiErr := f.S.ApproveDelivery(ctx, string(stream), api.DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash, Description: &edited})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if approval.Description != edited || approval.Title != presented.Title {
		t.Fatalf("approval %+v, want the edited body and the presented title", approval)
	}
}

func TestDeliveryApprovalIsStaleWhenTheStatusGoalChangesBeforeApproval(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.DeliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	first, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	fixturekit.SetStatusGoal(t, repository, stream, "A new status goal", time.Now().UTC())
	if _, apiErr := f.S.ApproveDelivery(ctx, string(stream), api.DeliveryDecision{Review: first.Report.Review, ReviewRevision: first.ReviewRevision, Commit: first.Report.Commit, DraftHash: first.DraftHash}); apiErr == nil || apiErr.Code != api.Conflict {
		t.Fatalf("approved a presentation whose title the owner was not shown: %v", apiErr)
	}
	second, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if second.Title != "A new status goal" || second.DraftHash == first.DraftHash {
		t.Fatalf("fresh presentation %+v does not show the new title", second)
	}
	approval, apiErr := f.S.ApproveDelivery(ctx, string(stream), api.DeliveryDecision{Review: second.Report.Review, ReviewRevision: second.ReviewRevision, Commit: second.Report.Commit, DraftHash: second.DraftHash})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if approval.Title != "A new status goal" {
		t.Fatalf("approval %+v did not record the new title", approval)
	}
}

func TestPublicationOpensThePullRequestWithTheStatusGoalAsItsTitle(t *testing.T) {
	t.Parallel()
	p := journey.NewUnpushedPublicationFixture(t, "commit-per-unit", "")
	ctx := context.Background()
	long := strings.Repeat("Ship the resumable upload feature to every customer ", 3)
	fixturekit.SetStatusGoal(t, p.Repository, p.Stream, long, p.S.Now())
	p.PublishDraft(t)
	p.Approve(t, nil)
	op := p.Request(t)
	result, err := p.Publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	want := string([]rune(long)[:execution.SubjectLength-1]) + "…"
	if runes := []rune(want); len(runes) != execution.SubjectLength || len(runes) >= len([]rune(long)) {
		t.Fatalf("fixture goal %q was not long enough to exercise the bound", long)
	}
	if len(p.Pulls.Prs) != 1 || p.Pulls.Prs[0].Title != want {
		t.Fatalf("pull request title %q, want the bounded status goal %q, not the final review summary %q", p.Pulls.Prs[0].Title, want, p.Report.Summary)
	}
}

func TestPublicationUsesTheApprovedTitleDespiteALaterStatusGoalChange(t *testing.T) {
	t.Parallel()
	p := journey.NewPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	approval := p.Approve(t, nil)
	if approval.Title != "Resumable uploads" {
		t.Fatalf("approval title %q", approval.Title)
	}
	fixturekit.SetStatusGoal(t, p.Repository, p.Stream, "A completely different later goal", p.S.Now())
	op := p.Request(t)
	result, err := p.Publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	if len(p.Pulls.Prs) != 1 || p.Pulls.Prs[0].Title != "Resumable uploads" {
		t.Fatalf("pull request %+v, want the approved title unaffected by the later status goal", p.Pulls.Prs)
	}
}

func TestSameRepositoryMaintenanceUpdatesAnExistingPullRequestsTitle(t *testing.T) {
	t.Parallel()
	p := sameRepositoryMaintenance(t)
	before := p.Pulls.Prs[0].Title
	fixturekit.SetStatusGoal(t, p.Repository, p.Stream, "A refreshed delivery title", p.S.Now())
	approveMaintenance(t, p)
	if before == "A refreshed delivery title" {
		t.Fatal("fixture already used the new title")
	}
	if p.Pulls.Prs[0].Title != before {
		t.Fatalf("the live pull request's title moved before publication: %q", p.Pulls.Prs[0].Title)
	}
	op := fixturekit.ActionOperation(t, p.Repository, p.Stream, lifecycle.PublishUpstreamAction)
	in, err := delivery.DecodePublish(op)
	fixturekit.Must(t, err)
	if in.Retarget == nil || in.Retarget.Title != before {
		t.Fatalf("retarget %+v, want the prior title %q", in.Retarget, before)
	}
	result, err := p.Publisher().Apply(context.Background(), op)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" || p.Pulls.Updates != 1 {
		t.Fatalf("publication %+v, updates %d", result, p.Pulls.Updates)
	}
	if p.Pulls.Prs[0].Title != "A refreshed delivery title" {
		t.Fatalf("pull request title %q, want it updated to the approved title", p.Pulls.Prs[0].Title)
	}
}

func TestPresentingTheSameTraceTwiceIsDeterministicAndCallsNoAgent(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := journey.DeliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	first, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	second, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if first.Title != second.Title || first.Draft != second.Draft || first.DraftHash != second.DraftHash {
		t.Fatalf("two presentations of the same trace differ: %+v, %+v", first, second)
	}
}
