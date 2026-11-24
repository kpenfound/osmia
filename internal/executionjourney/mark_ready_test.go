package executionjourney

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func (d *draftFixture) recordBrief(t *testing.T, items ...string) {
	t.Helper()
	report, _, err := delivery.LatestFinalReport(d.repository, d.stream)
	fixturekit.Must(t, err)
	review, _, _, err := delivery.DeliveryDocuments(d.repository, d.stream)
	fixturekit.Must(t, err)
	brief := delivery.Brief{Review: report.Review, ReviewRevision: review.Revision, Commit: report.Commit, Text: "Uploads resume after a restart.", BeforeYouShip: items}
	content, err := json.Marshal(brief)
	fixturekit.Must(t, err)
	revision, err := intake.NextRevision(d.repository, d.stream, delivery.BriefDocument(brief.Review))
	fixturekit.Must(t, err)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.BriefDocument(brief.Review), Revision: revision, Project: d.repository.Project(), Workstream: d.stream, At: d.f.S.Now(), Actor: intake.ChiefActor, Cause: "fixture"},
		Path: delivery.BriefPath(brief.Review), Content: string(content) + "\n"}
	fixturekit.Must(t, d.repository.RecordDocuments(context.Background(), []trace.Document{doc}))
}

func (d *draftFixture) present(t *testing.T) api.DeliveryPresentation {
	t.Helper()
	presented, apiErr := d.f.S.DeliveryPresentation(context.Background(), string(d.stream))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	return presented
}

func (d *draftFixture) decide(presented api.DeliveryPresentation) (api.DeliveryApproval, *api.APIError) {
	return d.f.S.ApproveDelivery(context.Background(), string(d.stream), api.DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
}

func (d *draftFixture) settleReady(t *testing.T) {
	t.Helper()
	p := &journey.Publisher{S: d.f.S, Repository: d.repository}
	op := fixturekit.RequestPublication(t, d.repository, d.stream, delivery.PublishAction, p.Pass)
	if result := journey.SettleOperation(t, d.f.S, d.repository, d.stream, op, p); result.Outcome != "succeeded" {
		t.Fatalf("marking ready %+v", result)
	}
}

func TestApprovingMarkReadyUpdatesTheDraftMarksItReadyAndDeliversTheWorkstream(t *testing.T) {
	t.Parallel()
	d := newDraftFixture(t, "commit-per-unit")
	ctx := context.Background()
	judge := &jevtest.Provider{}
	d.f.S.Jev = &jev.Judge{Config: func() config.Jev { return d.f.S.Cfg.Jev }, Provider: judge.Factory(), Getenv: func(string) string { return "key" }, Now: d.f.S.Now}
	d.read(1)
	if result := d.settle(t, d.first); result.Outcome != "succeeded" {
		t.Fatalf("final review %+v", result)
	}
	d.recordBrief(t, "Set OSMIA_TOKEN", "Restart the service")
	pushed, _ := d.forkBranch(t)
	pushes := d.pushCount(t)
	draftBody := d.pulls.Prs[0].Body
	if strings.Contains(draftBody, "Before you ship") {
		t.Fatalf("the draft already lists the items: %q", draftBody)
	}

	presented := d.present(t)
	if presented.Checks == nil || presented.Checks.State != api.ChecksStatePassed || presented.Checks.Summary != "Checks passed" {
		t.Fatalf("the presented check result %+v", presented.Checks)
	}
	if presented.Brief == nil || presented.Brief.State != api.BriefRecorded || presented.Publication == nil || presented.Publication.PullRequest != d.pulls.Prs[0].Number {
		t.Fatalf("the presentation lacks the brief or the draft's pull request: %+v %+v", presented.Brief, presented.Publication)
	}
	if !strings.Contains(presented.Draft, "Before you ship:\n- Set OSMIA_TOKEN\n- Restart the service\n") {
		t.Fatalf("the description to publish lacks the items: %q", presented.Draft)
	}
	entry, found, err := d.f.S.Delivery().DeliveryEntry(ctx, d.repository, d.stream)
	fixturekit.Must(t, err)
	if !found || !strings.HasPrefix(entry.Question, "Mark ready for review: ") || !strings.Contains(entry.Question, "Checks passed.") || entry.Blocked != "Marking the pull request ready for review." {
		t.Fatalf("the inbox entry %+v", entry)
	}

	approval, apiErr := d.decide(presented)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if approval.Description != presented.Draft || d.pulls.Readies != 0 || d.pulls.Updates != 0 {
		t.Fatalf("approving changed the pull request before it was applied: %+v, %d ready calls, %d updates", approval, d.pulls.Readies, d.pulls.Updates)
	}
	d.settleReady(t)

	pr := d.pulls.Prs[0]
	if len(d.pulls.Prs) != 1 || d.pulls.Creates != 1 || d.pulls.Updates != 1 || d.pulls.Readies != 1 || pr.Draft || pr.State != "open" || pr.Body != approval.Description || pr.Title != presented.Title {
		t.Fatalf("pull requests %+v, %d created, %d updated, %d marked ready", d.pulls.Prs, d.pulls.Creates, d.pulls.Updates, d.pulls.Readies)
	}
	if !strings.Contains(pr.Body, "Before you ship:\n- Set OSMIA_TOKEN\n- Restart the service\n") {
		t.Fatalf("the pull request lacks the items: %q", pr.Body)
	}
	if tip, _ := d.forkBranch(t); tip != pushed || d.pushCount(t) != pushes {
		t.Fatalf("marking ready pushed: the branch is at %s, was %s, %d pushes, were %d", tip, pushed, d.pushCount(t), pushes)
	}
	if got := d.feature(t); got != intake.DeliveredState {
		t.Fatalf("the workstream is %s, want delivered", got)
	}
	ready, recorded, err := delivery.ReadyAt(d.repository, d.stream)
	fixturekit.Must(t, err)
	if !recorded || ready.Source != api.ReadyByApproval || ready.PullRequest != pr.Number || ready.Approval != 1 || ready.Commit != pr.HeadCommit {
		t.Fatalf("the ready record %+v", ready)
	}
	if n := len(judge.Requests()); n != 0 {
		t.Fatalf("Jev was asked %d times", n)
	}

	delivered := latestOf(t, d.repository, d.stream, trace.FeatureSubject)
	if delivered.To != intake.DeliveredState || delivered.DecidedBy != trace.DecidedByOwner || delivered.Judgment != "" || delivered.DefaultTo != intake.DeliveredState || delivered.Departed() {
		t.Fatalf("the delivered transition %+v", delivered)
	}
	if rec := journey.RouteOf(t, d.repository, d.stream, intake.TriggerDeliveryApproved); rec.Authority != router.AuthorityOwner || rec.Rule != "pinned" || rec.Outcome != router.OutcomeFollowed || rec.JevRequested || rec.State != router.StateCommitted || rec.Target != intake.TargetDelivered {
		t.Fatalf("route %+v", rec)
	}

	operations := len(fixturekit.PublicationOperations(t, d.repository, d.stream, delivery.PublishAction))
	fixturekit.Must(t, (&journey.Publisher{S: d.f.S, Repository: d.repository}).Pass(ctx))
	if got := len(fixturekit.PublicationOperations(t, d.repository, d.stream, delivery.PublishAction)); got != operations || d.pulls.Readies != 1 || d.pulls.Updates != 1 {
		t.Fatalf("a later pass asked again: %d operations, were %d; %d ready calls, %d updates", got, operations, d.pulls.Readies, d.pulls.Updates)
	}
}

func TestAnApprovalOfAReplacedPushIsRefusedAsStale(t *testing.T) {
	t.Parallel()
	d := newDraftFixture(t, "commit-per-unit")
	d.read(1)
	if result := d.settle(t, d.first); result.Outcome != "succeeded" {
		t.Fatalf("first final review %+v", result)
	}
	stale := d.present(t)

	second := d.review(t, map[string]string{"internal/trace/revision.go": "package trace\n\n// Revision answers a finding.\n"})
	d.read(2)
	if result := d.settle(t, second); result.Outcome != "succeeded" {
		t.Fatalf("second final review %+v", result)
	}
	if _, apiErr := d.decide(stale); apiErr == nil || apiErr.Code != api.Conflict {
		t.Fatalf("the approval of the replaced push: %v", apiErr)
	}
	d.noApproval(t)
	if d.pulls.Readies != 0 || !d.pulls.Prs[0].Draft || d.feature(t) != intake.AssembledState {
		t.Fatalf("the stale approval changed the pull request or the workstream: %+v, %s", d.pulls.Prs[0], d.feature(t))
	}

	fresh := d.present(t)
	if _, apiErr := d.decide(fresh); apiErr != nil {
		t.Fatal(apiErr)
	}
	third := d.review(t, map[string]string{"internal/trace/again.go": "package trace\n\n// Again answers another finding.\n"})
	d.read(3)
	if result := d.settle(t, third); result.Outcome != "succeeded" {
		t.Fatalf("third final review %+v", result)
	}
	p := &journey.Publisher{S: d.f.S, Repository: d.repository}
	before := len(fixturekit.PublicationOperations(t, d.repository, d.stream, delivery.PublishAction))
	fixturekit.Must(t, p.Pass(context.Background()))
	if after := len(fixturekit.PublicationOperations(t, d.repository, d.stream, delivery.PublishAction)); after != before || d.pulls.Readies != 0 || !d.pulls.Prs[0].Draft {
		t.Fatalf("an approval of a replaced push was applied: %d operations, were %d, %d ready calls", after, before, d.pulls.Readies)
	}
}
