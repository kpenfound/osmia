package executionjourney

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

func latestOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, subject string) trace.Transition {
	t.Helper()
	var out trace.Transition
	for _, tr := range intakefixture.WorkTransitions(t, repo, stream) {
		if tr.Subject == subject {
			out = tr
		}
	}
	return out
}

func TestAUnitExitThatFitsItsDefaultCommitsItAndRecordsIt(t *testing.T) {
	t.Parallel()
	f, stream, repo := newChecksFixture(t, "route-default", &fixturekit.FakeChecks{Outcomes: fixturekit.Passing()})
	p := &jevfixture.Transitions{}
	log := journey.RouteThrough(f.S, p)
	journey.RunChecks(t, f.S, repo, stream)

	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != intake.UnitReviewing || moved.DefaultTo != intake.UnitReviewing || moved.DecidedBy != trace.DecidedByCode || moved.Departed() || moved.Judgment == "" {
		t.Fatalf("the unit's move to reviewing %+v", moved)
	}
	if !strings.HasPrefix(moved.ID, trace.UnitSubject("resume")+"-"+intake.UnitReviewing+"-checks-") || strings.Contains(moved.Reason, "decide-transition") {
		t.Fatalf("a default that was followed reads as the controller wrote it: %+v", moved)
	}
	rec := journey.RouteOf(t, repo, stream, "unit.checks_passed")
	if rec.State != router.StateCommitted || rec.Outcome != router.OutcomeFollowed || rec.Default != intake.UnitReviewing || rec.Target != intake.UnitReviewing {
		t.Fatalf("route %+v", rec)
	}
	if len(p.TransitionRequests()) != 1 {
		t.Fatalf("Jev was asked %d times, want once for the one exit", len(p.TransitionRequests()))
	}
	var span *router.SpanRoute
	for _, s := range log.All() {
		if s.Trigger == "unit.checks_passed" {
			span = &s
		}
	}
	if span == nil || span.Outcome != router.OutcomeFollowed || span.Default != intake.UnitReviewing || span.Chosen != intake.UnitReviewing || span.Unit != "resume" {
		t.Fatalf("route span %+v", span)
	}
}

func TestASatisfactoryVerdictApprovesThroughTheRouter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := journey.NewReviewFixture(t, "route-verdict")
	p := &jevfixture.Transitions{}
	journey.RouteThrough(f.S, p)
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	state := intakefixture.UnitWorkflow(t, repo, stream, "resume")
	_, identity, err := r.PrepareUnitReview(ctx, stream, "resume")
	fixturekit.Must(t, err)
	result := service.UnitReviewResult{Identity: identity, Turn: execution.ReviewTurnID("resume", state.Version), Verdict: service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}}
	recordVerdict(t, repo, stream, result)

	fixturekit.Must(t, r.Driver().ApplyReview(ctx, stream, "resume", state, result))

	if got := intakefixture.UnitWorkflow(t, repo, stream, "resume"); got.Value != intake.UnitApproved {
		t.Fatalf("the unit is %s", got.Value)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != intake.UnitApproved || moved.DecidedBy != trace.DecidedByReviewer || moved.DefaultTo != intake.UnitApproved || moved.Departed() {
		t.Fatalf("the move to approved %+v", moved)
	}
	rec := journey.RouteOf(t, repo, stream, "unit.review_approved")
	if rec.Authority != router.AuthorityReviewer || !strings.HasPrefix(rec.AuthorityRef, execution.ReviewDocument("resume")+" revision ") || rec.State != router.StateCommitted {
		t.Fatalf("the approval's authority is not the reviewer's verdict: %+v", rec)
	}
	if _, _, ok, err := (&journey.Foreman{Masons: r.Masons}).Approval(stream, "resume", 0); err != nil || !ok {
		t.Fatalf("the approval record the foreman lands: %v %v", ok, err)
	}
}

func TestAVerdictForASupersededCandidateDoesNotApprove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := journey.NewReviewFixture(t, "route-superseded")
	journey.RouteThrough(f.S, &jevfixture.Transitions{})
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	state := intakefixture.UnitWorkflow(t, repo, stream, "resume")
	_, identity, err := r.PrepareUnitReview(ctx, stream, "resume")
	fixturekit.Must(t, err)
	old := service.UnitReviewResult{Identity: identity, Turn: execution.ReviewTurnID("resume", state.Version), Verdict: service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}}
	recordVerdict(t, repo, stream, old)

	newer := service.UnitReviewResult{Identity: identity, Turn: execution.ReviewTurnID("resume", state.Version) + "-recover-1", Verdict: service.UnitVerdict{Decision: "material_findings", Summary: fixturekit.ReviewSummary,
		Findings: []api.ReviewFinding{{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}}}, Bounces: 1}
	recordVerdict(t, repo, stream, newer)

	fixturekit.Must(t, r.Driver().ApplyReview(ctx, stream, "resume", state, old))

	if got := intakefixture.UnitWorkflow(t, repo, stream, "resume"); got.Value != intake.UnitReviewing || got.Version != state.Version {
		t.Fatalf("a superseded verdict moved the unit to %+v", got)
	}
	rec := journey.RouteOf(t, repo, stream, "unit.review_approved")
	if rec.State == router.StateCommitted || rec.Outcome != router.OutcomeHeld || rec.Hold != router.HoldDefaultIllegal || !strings.Contains(rec.Reason, "later review") {
		t.Fatalf("route %+v", rec)
	}
	notices := 0
	outbox, err := repo.Outbox(stream)
	fixturekit.Must(t, err)
	for _, e := range outbox {
		if e.Event.Kind == trace.NoticeKind && strings.HasPrefix(e.TransitionID, "route-") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("%d notices for the held exit, want one", notices)
	}
}

func recordVerdict(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, result service.UnitReviewResult) {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	var latest trace.Document
	for _, d := range docs {
		if d.ID == execution.ReviewDocument("resume") {
			latest = d
		}
	}
	data, err := json.MarshalIndent(result, "", "  ")
	fixturekit.Must(t, err)
	latest.Revision++
	latest.Content = string(data) + "\n"
	latest.Cause = result.Turn
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{latest}))
}

func TestAScriptedDepartureSendsTheWorkBackToTheMasonInsteadOfContesting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := journey.NewReviewFixture(t, "route-departure")
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		return jevfixture.Departs(a, intake.UnitImplementing, 0.95, 0.95)
	}}
	log := journey.RouteThrough(f.S, p)
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	state := intakefixture.UnitWorkflow(t, repo, stream, "resume")
	_, identity, err := r.PrepareUnitReview(ctx, stream, "resume")
	fixturekit.Must(t, err)
	reported := lastTurn(t, repo, stream, intake.MasonAgent("resume"))
	findings := []api.ReviewFinding{{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}}
	result := service.UnitReviewResult{Identity: identity, Turn: execution.ReviewTurnID("resume", state.Version), Bounces: f.S.Cfg.Shed.MaxBounces, Verdict: service.UnitVerdict{Decision: "material_findings", Summary: fixturekit.ReviewSummary, Findings: findings}}

	fixturekit.Must(t, r.Driver().ApplyReview(ctx, stream, "resume", state, result))

	if got := intakefixture.UnitWorkflow(t, repo, stream, "resume"); got.Value != intake.UnitImplementing {
		t.Fatalf("the unit is %s, want implementing", got.Value)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != intake.UnitImplementing || moved.DefaultTo != intake.UnitContested || moved.DecidedBy != trace.DecidedByJev || moved.Judgment == "" || !moved.Departed() {
		t.Fatalf("the departure %+v", moved)
	}
	if !execution.IsMove(moved, "resume") || !strings.Contains(moved.Reason, "decide-transition table") {
		t.Fatalf("the departure is entered through the shared effects of a move: %+v", moved)
	}
	ruling, found, err := execution.LatestMasonRuling(repo, stream, "resume")
	fixturekit.Must(t, err)
	if !found || ruling.ResetTurn != reported.Sequence {
		t.Fatalf("the mason's fresh allowance %+v", ruling)
	}
	rec := journey.RouteOf(t, repo, stream, "unit.review_rejected")
	if rec.Outcome != router.OutcomeDeparted || rec.DecidedBy != trace.DecidedByJev || rec.Judgment != moved.Judgment || rec.Default != intake.UnitContested || rec.Target != intake.UnitImplementing {
		t.Fatalf("route %+v", rec)
	}
	var span *router.SpanRoute
	for _, s := range log.All() {
		if s.Trigger == "unit.review_rejected" {
			span = &s
		}
	}
	if span == nil || span.Outcome != router.OutcomeDeparted || span.Default != intake.UnitContested || span.Chosen != intake.UnitImplementing || span.Judgment != moved.Judgment {
		t.Fatalf("route span %+v", span)
	}
	f.S.SetSole(&journey.ActiveProject{Repository: repo})
	feed, apiErr := service.InspectForTest(f.S.Core()).Feed(string(stream))
	if apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	departed := 0
	for _, e := range feed.Entries {
		if e.Transition != nil && e.Transition.Departed {
			departed++
			if e.Transition.DecidedBy != trace.DecidedByJev || e.Transition.DefaultTo != intake.UnitContested || e.Transition.To != intake.UnitImplementing {
				t.Fatalf("feed entry %+v", e.Transition)
			}
		}
		if e.Transition != nil && router.IsJournal(e.Transition.Subject) {
			t.Fatalf("the router's journal is in the feed: %+v", e.Transition)
		}
	}
	if departed != 1 {
		t.Fatalf("%d departures in the feed, want one", departed)
	}
}

func TestAnOwnerApprovalIsAPinnedMove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "route-owner-approval")
	p := &jevfixture.Transitions{}
	journey.RouteThrough(f.S, p)
	out, apiErr := f.S.Execution().OwnerMove(ctx, string(stream), "resume", (api.UnitMoveRequest{To: intake.UnitApproved, Note: "The owner read the candidate."}))
	if apiErr != nil {
		t.Fatal(apiErr.Message)
	}
	if out.Move.To != intake.UnitApproved || out.Move.By != "owner" {
		t.Fatalf("move %+v", out.Move)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != intake.UnitApproved || moved.DecidedBy != trace.DecidedByOwner || moved.DefaultTo != intake.UnitApproved || moved.Departed() {
		t.Fatalf("the owner's approval %+v", moved)
	}
	rec := journey.RouteOf(t, repo, stream, "unit.move_owner")
	if rec.Authority != router.AuthorityOwner || rec.Outcome != router.OutcomeFollowed || rec.Rule != "pinned" || rec.JevRequested {
		t.Fatalf("route %+v", rec)
	}
	if len(p.Requests()) != 0 {
		t.Fatalf("Jev was asked about an owner's decision: %d requests", len(p.Requests()))
	}
	if _, _, ok, err := (&journey.Foreman{Masons: journey.NewMasonController(f.S, repo)}).Approval(stream, "resume", 0); err != nil || !ok {
		t.Fatalf("the owner's approval record: %v %v", ok, err)
	}
}

func TestAChiefMoveToApprovedIsNeverLegal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "route-chief-approval")
	journey.RouteThrough(f.S, &jevfixture.Transitions{})
	before := intakefixture.UnitWorkflow(t, repo, stream, "resume")
	x, err := f.S.Execution().UnitExit(execution.UnitExitInput{Repo: repo, Stream: stream, Unit: "resume", Trigger: "unit.move_proposal", Cause: "chief-proposal-1",
		Default: intake.UnitApproved, Note: "Looks fine.", Actor: intake.ChiefActor, Active: true})
	fixturekit.Must(t, err)
	res, err := f.S.Core().RouteExit(ctx, x)
	fixturekit.Must(t, err)
	if res.Committed || res.Outcome != router.OutcomeHeld || res.Hold != router.HoldGate || res.Journal != router.StateOwner {
		t.Fatalf("the router entered approved on a chief proposal: %+v", res)
	}
	if got := intakefixture.UnitWorkflow(t, repo, stream, "resume"); got != before {
		t.Fatalf("the unit moved to %+v", got)
	}
}
