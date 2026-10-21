package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// setStatusGoal records goal as the workstream's chief-of-staff status, the
// value workstreamTitle (and archiveTitle and status) read as the
// workstream's title.
func setStatusGoal(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, goal string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	_, err := repository.EnsureChiefOfStaff(ctx, stream, at, serviceActor)
	must(t, err)
	turn := fmt.Sprintf("status-%d", at.UnixNano())
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request-" + turn, Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: serviceActor, Cause: "fixture"}
	_, err = repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: turn, Profile: coreadapter.Profile{Name: "fake", Backend: "fake", Model: "fake"}, Prompt: "Set status"})
	must(t, err)
	session := t.TempDir()
	_, err = repository.ClaimTurn(ctx, stream, trace.ChiefOfStaff, turn+"-token", session, at)
	must(t, err)
	_, err = repository.SetStatus(ctx, trace.ChiefOfStaff, coreadapter.Scope{Project: string(repository.Project()), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: turn},
		trace.StatusContent{Goal: goal, Note: "status", Agents: []string{}}, at, func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	must(t, err)
}

// recordHanded records a handed document for stream with the given recorded
// source, as hand-in would, so draftDelivery can read it.
func recordHanded(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, source string, at time.Time) {
	t.Helper()
	must(t, repository.Append(context.Background(), trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: handedDocument, Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: ownerActor, Cause: "fixture"},
		Path: "handed/input.md", Content: "# Handed issue title\n\nBody.\n", Source: source}))
}

// TestDraftDeliveryTitleIsTheWorkstreamStatusGoal shows the drafted title is
// the latest chief-of-staff status goal, not text taken from the final
// review's summary (spec#1).
func TestDraftDeliveryTitleIsTheWorkstreamStatusGoal(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	defer repository.Close()
	setStatusGoal(t, repository, stream, "Ship resumable uploads to users", time.Now().UTC())
	title, _, err := draftDelivery(f.s.about(repository), repository, stream, report)
	must(t, err)
	if title != "Ship resumable uploads to users" {
		t.Fatalf("title %q, want the status goal, not the final review's summary %q", title, report.Summary)
	}
}

// TestDraftDeliveryTitleFallsBackToSpecHeadingWithoutAGoal shows that a
// workstream with no status goal gets a meaningful fallback title (the
// sealed spec's first heading) before the bare workstream ID (spec#2).
func TestDraftDeliveryTitleFallsBackToSpecHeadingWithoutAGoal(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	defer repository.Close()
	title, _, err := draftDelivery(f.s.about(repository), repository, stream, report)
	must(t, err)
	if title != "Resumable uploads" {
		t.Fatalf("title %q, want the spec's first heading", title)
	}
	if title == "Workstream "+string(stream) {
		t.Fatal("fell all the way back to the bare workstream ID")
	}
}

// TestDraftDeliveryBodyIsASpecSummaryWithNoWorkAccounting shows the drafted
// body is a short prose summary of the sealed spec, with no per-criterion
// evidence or gap lines, no landed-unit list and no reviewed commit hash
// (spec#3).
func TestDraftDeliveryBodyIsASpecSummaryWithNoWorkAccounting(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	defer repository.Close()
	_, body, err := draftDelivery(f.s.about(repository), repository, stream, report)
	must(t, err)
	if !strings.Contains(body, "An interrupted upload resumes from its last acknowledged chunk") {
		t.Fatalf("body %q does not hold the spec's opening prose", body)
	}
	for _, unwanted := range []string{report.Commit, "spec#1", "spec#2", "Landed units", "resume.go", "dedupe.go", "Evidence:", "Gap:"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("body %q still accounts for the work with %q", body, unwanted)
		}
	}
}

// TestDraftDeliveryClosesAnIssueInTheTargetRepository shows that a
// workstream handed in from a GitHub issue in the repository the pull
// request targets gets a "closes #N" reference (spec#4).
func TestDraftDeliveryClosesAnIssueInTheTargetRepository(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	defer repository.Close()
	recordHanded(t, repository, stream, "https://github.com/dagger/dagger/issues/42", time.Now().UTC())
	_, body, err := draftDelivery(f.s.about(repository), repository, stream, report)
	must(t, err)
	if !strings.Contains(body, "closes #42") {
		t.Fatalf("body %q does not close the same-repository issue", body)
	}
}

// TestDraftDeliveryClosesAnIssueInAnotherRepository shows that a workstream
// handed in from a GitHub issue in a different repository than the pull
// request targets gets a "closes OWNER/REPO#N" reference (spec#5).
func TestDraftDeliveryClosesAnIssueInAnotherRepository(t *testing.T) {
	t.Parallel()
	f, stream, repository, report := deliveryFixture(t)
	defer repository.Close()
	recordHanded(t, repository, stream, "https://github.com/acme/other/issues/7", time.Now().UTC())
	_, body, err := draftDelivery(f.s.about(repository), repository, stream, report)
	must(t, err)
	if !strings.Contains(body, "closes acme/other#7") {
		t.Fatalf("body %q does not close the other-repository issue", body)
	}
}

// TestDraftDeliveryNoClosingReferenceForFileOrStdin shows that a workstream
// handed in from a file or stdin gets no closing issue reference (spec#6).
func TestDraftDeliveryNoClosingReferenceForFileOrStdin(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"file:/home/owner/design.md", "stdin"} {
		t.Run(source, func(t *testing.T) {
			f, stream, repository, report := deliveryFixture(t)
			defer repository.Close()
			recordHanded(t, repository, stream, source, time.Now().UTC())
			_, body, err := draftDelivery(f.s.about(repository), repository, stream, report)
			must(t, err)
			if strings.Contains(body, "closes") {
				t.Fatalf("body %q closes an issue for a %s hand-in", body, source)
			}
		})
	}
}

// TestDeliveryPresentationShowsTheTitleAndKeepsTheBodyEditable shows that
// the presentation carries the title to be published alongside the drafted
// body, the final review report with every criterion's evidence or gap
// (spec#9), and that the owner's edited body, not the title, is what gets
// approved (spec#7).
func TestDeliveryPresentationShowsTheTitleAndKeepsTheBodyEditable(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := deliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	presented, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil {
		t.Fatal(api)
	}
	if presented.Title != "Resumable uploads" {
		t.Fatalf("presentation title %q", presented.Title)
	}
	if len(presented.Report.Criteria) != 2 || presented.Report.Criteria[0].Evidence == "" || presented.Report.Criteria[1].Evidence == "" {
		t.Fatalf("presentation report %+v does not carry every criterion's evidence", presented.Report)
	}
	edited := "Owner's own words about the change.\n"
	approval, api := f.s.approveDelivery(ctx, string(stream), DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash, Description: &edited})
	if api != nil {
		t.Fatal(api)
	}
	if approval.Description != edited || approval.Title != presented.Title {
		t.Fatalf("approval %+v, want the edited body and the presented title", approval)
	}
}

// TestDeliveryApprovalIsStaleWhenTheStatusGoalChangesBeforeApproval shows
// that approving the earlier presentation after the status goal changes is
// rejected as stale, and that the fresh presentation shows the new title,
// which an approval then records (spec#7, spec#8).
func TestDeliveryApprovalIsStaleWhenTheStatusGoalChangesBeforeApproval(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := deliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	first, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil {
		t.Fatal(api)
	}
	setStatusGoal(t, repository, stream, "A new status goal", time.Now().UTC())
	if _, api := f.s.approveDelivery(ctx, string(stream), DeliveryDecision{Review: first.Report.Review, ReviewRevision: first.ReviewRevision, Commit: first.Report.Commit, DraftHash: first.DraftHash}); api == nil || api.Code != Conflict {
		t.Fatalf("approved a presentation whose title the owner was not shown: %v", api)
	}
	second, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil {
		t.Fatal(api)
	}
	if second.Title != "A new status goal" || second.DraftHash == first.DraftHash {
		t.Fatalf("fresh presentation %+v does not show the new title", second)
	}
	approval, api := f.s.approveDelivery(ctx, string(stream), DeliveryDecision{Review: second.Report.Review, ReviewRevision: second.ReviewRevision, Commit: second.Report.Commit, DraftHash: second.DraftHash})
	if api != nil {
		t.Fatal(api)
	}
	if approval.Title != "A new status goal" {
		t.Fatalf("approval %+v did not record the new title", approval)
	}
}

// TestPublicationOpensThePullRequestWithTheStatusGoalAsItsTitle shows that
// the pull request a publication opens has the workstream's status goal as
// its title, within the existing title length bound, and not text from the
// final review's summary (spec#1).
func TestPublicationOpensThePullRequestWithTheStatusGoalAsItsTitle(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	long := strings.Repeat("Ship the resumable upload feature to every customer ", 3)
	setStatusGoal(t, p.repository, p.stream, long, p.s.now())
	p.approve(t, nil)
	op := p.request(t)
	result, err := p.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	want := boundSubject(long)
	if runes := []rune(want); len(runes) != subjectLength || len(runes) >= len([]rune(long)) {
		t.Fatalf("fixture goal %q was not long enough to exercise the bound", long)
	}
	if len(p.pulls.Prs) != 1 || p.pulls.Prs[0].Title != want {
		t.Fatalf("pull request title %q, want the bounded status goal %q, not the final review summary %q", p.pulls.Prs[0].Title, want, p.report.Summary)
	}
}

// TestPublicationUsesTheApprovedTitleDespiteALaterStatusGoalChange shows
// that the pull request a publication opens uses the title the owner
// approved, even though the workstream's status goal changes afterwards
// (spec#8).
func TestPublicationUsesTheApprovedTitleDespiteALaterStatusGoalChange(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	approval := p.approve(t, nil)
	if approval.Title != "Resumable uploads" {
		t.Fatalf("approval title %q", approval.Title)
	}
	setStatusGoal(t, p.repository, p.stream, "A completely different later goal", p.s.now())
	op := p.request(t)
	result, err := p.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	if len(p.pulls.Prs) != 1 || p.pulls.Prs[0].Title != "Resumable uploads" {
		t.Fatalf("pull request %+v, want the approved title unaffected by the later status goal", p.pulls.Prs)
	}
}

// TestSameRepositoryMaintenanceUpdatesAnExistingPullRequestsTitle shows that
// an existing pull request whose body matches the approved description but
// whose title differs from the approved title is not treated as already
// matching: the next maintenance pass updates it to the approved title
// (spec#8).
func TestSameRepositoryMaintenanceUpdatesAnExistingPullRequestsTitle(t *testing.T) {
	t.Parallel()
	p := sameRepositoryMaintenance(t)
	before := p.pulls.Prs[0].Title
	setStatusGoal(t, p.repository, p.stream, "A refreshed delivery title", p.s.now())
	approveMaintenance(t, p)
	if before == "A refreshed delivery title" {
		t.Fatal("fixture already used the new title")
	}
	if p.pulls.Prs[0].Title != before {
		t.Fatalf("the live pull request's title moved before publication: %q", p.pulls.Prs[0].Title)
	}
	op := actionOperation(t, p.repository, p.stream, publishUpstreamAction)
	in, err := decodePublish(op)
	must(t, err)
	if in.Retarget == nil || in.Retarget.Title != before {
		t.Fatalf("retarget %+v, want the prior title %q", in.Retarget, before)
	}
	result, err := p.publisher().Apply(context.Background(), op)
	must(t, err)
	if result.Outcome != "succeeded" || p.pulls.Updates != 1 {
		t.Fatalf("publication %+v, updates %d", result, p.pulls.Updates)
	}
	if p.pulls.Prs[0].Title != "A refreshed delivery title" {
		t.Fatalf("pull request title %q, want it updated to the approved title", p.pulls.Prs[0].Title)
	}
}

// recordLegacyApproval records, directly in the trace, a delivery approval
// of the shape recorded before the title became part of the draft: one
// with no Title field, as approveDelivery itself wrote before this change.
func recordLegacyApproval(t *testing.T, p *publicationFixture, presented DeliveryPresentation) DeliveryApproval {
	t.Helper()
	ctx := context.Background()
	r := presented.Report
	legacy := DeliveryApproval{Review: r.Review, ReviewRevision: presented.ReviewRevision, Commit: r.Commit, Seal: r.Seal, SpecHash: r.SpecHash, Spec: r.Spec, Plan: r.Plan, Charter: r.Charter,
		Description: presented.Draft, DescriptionHash: descriptionHash(presented.Draft), Messages: presented.Messages, Style: presented.Style, At: p.s.now()}
	content, err := json.Marshal(legacy)
	must(t, err)
	state, err := p.repository.Workflow(p.stream, deliverySubject)
	must(t, err)
	id := "legacy-delivery-owner-1"
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: deliveryDocument, Revision: 1, Project: p.repository.Project(), Workstream: p.stream, At: legacy.At, Actor: ownerActor, Cause: id}, Path: deliveryPath, Content: string(content)}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: ownerHeader(id, p.repository.Project(), p.stream, "delivery-approval", legacy.At), Subject: deliverySubject, From: state.Value, To: "approved-1", Reason: "legacy approval fixture"}}
	_, err = p.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx)
	must(t, err)
	return legacy
}

// TestPublicationPublishesALegacyApprovalWithTheDerivedTitle shows that a
// delivery approval recorded before this change, with no Title field,
// still publishes, sending and matching against today's derived title,
// deliveryTitle(description, stream) (spec#10).
func TestPublicationPublishesALegacyApprovalWithTheDerivedTitle(t *testing.T) {
	t.Parallel()
	p := newPublicationFixture(t, "commit-per-unit")
	ctx := context.Background()
	presented, api := p.s.deliveryPresentation(ctx, string(p.stream))
	if api != nil {
		t.Fatal(api)
	}
	legacy := recordLegacyApproval(t, p, presented)
	wantTitle := deliveryTitle(legacy.Description, p.stream)
	if wantTitle == presented.Title {
		t.Fatal("the derived title happens to equal the presented title; pick a draft where they differ")
	}
	op := p.request(t)
	result, err := p.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication of a legacy approval %+v: %v", result, err)
	}
	if len(p.pulls.Prs) != 1 || p.pulls.Prs[0].Title != wantTitle {
		t.Fatalf("pull request %+v, want the title derived from the legacy approval's description", p.pulls.Prs)
	}
	records, err := publications(p.repository, p.stream)
	must(t, err)
	if last := records[len(records)-1]; last.Title != wantTitle {
		t.Fatalf("publication record %+v, want the derived title", last)
	}
	// A retry still matches the pull request it already opened with that
	// derived title: it is not treated as mismatched and recreated.
	again, err := p.publisher().Apply(ctx, op)
	if err != nil || again.Outcome != "succeeded" || p.pulls.Creates != 1 {
		t.Fatalf("retry of a legacy publication %+v, %v, %d pull requests", again, err, p.pulls.Creates)
	}
}

// TestPresentingTheSameTraceTwiceIsDeterministicAndCallsNoAgent presents the
// same recorded trace twice and shows the draft and its hash are identical
// both times (spec#11). draftDelivery and workstreamTitle read only
// recorded trace documents and the workstream's status; neither reaches the
// committee engine or any other agent, so presenting makes no agent call.
func TestPresentingTheSameTraceTwiceIsDeterministicAndCallsNoAgent(t *testing.T) {
	t.Parallel()
	f, stream, repository, _ := deliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	first, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil {
		t.Fatal(api)
	}
	second, api := f.s.deliveryPresentation(ctx, string(stream))
	if api != nil {
		t.Fatal(api)
	}
	if first.Title != second.Title || first.Draft != second.Draft || first.DraftHash != second.DraftHash {
		t.Fatalf("two presentations of the same trace differ: %+v, %+v", first, second)
	}
}
