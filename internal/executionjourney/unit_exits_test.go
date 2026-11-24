package executionjourney

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

type spanLog struct {
	mu    sync.Mutex
	spans []router.SpanRoute
}

func (l *spanLog) add(s router.SpanRoute) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spans = append(l.spans, s)
}

func (l *spanLog) all() []router.SpanRoute {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]router.SpanRoute(nil), l.spans...)
}

func routeThrough(s *Service, p *jevfixture.Transitions) *spanLog {
	service.InspectForTest(s.core()).SetJudge(jevfixture.TransitionJudge(s.now, p))
	log := &spanLog{}
	s.core().Router().Spans = log.add
	return log
}

func routeRecords(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []router.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	latest := map[string]trace.Document{}
	var order []string
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "routes/") {
			continue
		}
		if _, ok := latest[d.ID]; !ok {
			order = append(order, d.ID)
		}
		if d.Revision >= latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	var out []router.Record
	for _, id := range order {
		var rec router.Record
		fixturekit.Must(t, json.Unmarshal([]byte(latest[id].Content), &rec))
		out = append(out, rec)
	}
	return out
}

func routeOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, trigger string) router.Record {
	t.Helper()
	for _, rec := range routeRecords(t, repo, stream) {
		if rec.Trigger == trigger {
			return rec
		}
	}
	t.Fatalf("no exit of trigger %s was routed: %+v", trigger, routeRecords(t, repo, stream))
	return router.Record{}
}

func latestOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, subject string) trace.Transition {
	t.Helper()
	var out trace.Transition
	for _, tr := range workTransitions(t, repo, stream) {
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
	log := routeThrough(f.s, p)
	runChecks(t, f.s, repo, stream)

	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != intake.UnitReviewing || moved.DefaultTo != intake.UnitReviewing || moved.DecidedBy != trace.DecidedByCode || moved.Departed() || moved.Judgment == "" {
		t.Fatalf("the unit's move to reviewing %+v", moved)
	}
	if !strings.HasPrefix(moved.ID, trace.UnitSubject("resume")+"-"+intake.UnitReviewing+"-checks-") || strings.Contains(moved.Reason, "decide-transition") {
		t.Fatalf("a default that was followed reads as the controller wrote it: %+v", moved)
	}
	rec := routeOf(t, repo, stream, "unit.checks_passed")
	if rec.State != router.StateCommitted || rec.Outcome != router.OutcomeFollowed || rec.Default != intake.UnitReviewing || rec.Target != intake.UnitReviewing {
		t.Fatalf("route %+v", rec)
	}
	if len(p.TransitionRequests()) != 1 {
		t.Fatalf("Jev was asked %d times, want once for the one exit", len(p.TransitionRequests()))
	}
	var span *router.SpanRoute
	for _, s := range log.all() {
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
	f, stream, repo := newReviewFixture(t, "route-verdict")
	p := &jevfixture.Transitions{}
	routeThrough(f.s, p)
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state := workflowOf(t, repo, stream, "resume")
	_, identity, err := r.prepareUnitReview(ctx, stream, "resume")
	fixturekit.Must(t, err)
	result := service.UnitReviewResult{Identity: identity, Turn: execution.ReviewTurnID("resume", state.Version), Verdict: service.UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}}
	recordVerdict(t, repo, stream, result)

	fixturekit.Must(t, r.applyReview(ctx, stream, "resume", state, result))

	if got := workflowOf(t, repo, stream, "resume"); got.Value != intake.UnitApproved {
		t.Fatalf("the unit is %s", got.Value)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != intake.UnitApproved || moved.DecidedBy != trace.DecidedByReviewer || moved.DefaultTo != intake.UnitApproved || moved.Departed() {
		t.Fatalf("the move to approved %+v", moved)
	}
	rec := routeOf(t, repo, stream, "unit.review_approved")
	if rec.Authority != router.AuthorityReviewer || !strings.HasPrefix(rec.AuthorityRef, execution.ReviewDocument("resume")+" revision ") || rec.State != router.StateCommitted {
		t.Fatalf("the approval's authority is not the reviewer's verdict: %+v", rec)
	}
	if _, _, ok, err := (&foreman{masons: r.masons}).approval(stream, "resume", 0); err != nil || !ok {
		t.Fatalf("the approval record the foreman lands: %v %v", ok, err)
	}
}

func TestAVerdictForASupersededCandidateDoesNotApprove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newReviewFixture(t, "route-superseded")
	routeThrough(f.s, &jevfixture.Transitions{})
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state := workflowOf(t, repo, stream, "resume")
	_, identity, err := r.prepareUnitReview(ctx, stream, "resume")
	fixturekit.Must(t, err)
	old := service.UnitReviewResult{Identity: identity, Turn: execution.ReviewTurnID("resume", state.Version), Verdict: service.UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}}
	recordVerdict(t, repo, stream, old)

	newer := service.UnitReviewResult{Identity: identity, Turn: execution.ReviewTurnID("resume", state.Version) + "-recover-1", Verdict: service.UnitVerdict{Decision: "material_findings", Summary: reviewSummary,
		Findings: []api.ReviewFinding{{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}}}, Bounces: 1}
	recordVerdict(t, repo, stream, newer)

	fixturekit.Must(t, r.applyReview(ctx, stream, "resume", state, old))

	if got := workflowOf(t, repo, stream, "resume"); got.Value != intake.UnitReviewing || got.Version != state.Version {
		t.Fatalf("a superseded verdict moved the unit to %+v", got)
	}
	rec := routeOf(t, repo, stream, "unit.review_approved")
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
	f, stream, repo := newReviewFixture(t, "route-departure")
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		return jevfixture.Departs(a, intake.UnitImplementing, 0.95, 0.95)
	}}
	log := routeThrough(f.s, p)
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state := workflowOf(t, repo, stream, "resume")
	_, identity, err := r.prepareUnitReview(ctx, stream, "resume")
	fixturekit.Must(t, err)
	reported := lastTurn(t, repo, stream, intake.MasonAgent("resume"))
	findings := []api.ReviewFinding{{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}}
	result := service.UnitReviewResult{Identity: identity, Turn: execution.ReviewTurnID("resume", state.Version), Bounces: f.s.cfg.Shed.MaxBounces, Verdict: service.UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: findings}}

	fixturekit.Must(t, r.applyReview(ctx, stream, "resume", state, result))

	if got := workflowOf(t, repo, stream, "resume"); got.Value != intake.UnitImplementing {
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
	rec := routeOf(t, repo, stream, "unit.review_rejected")
	if rec.Outcome != router.OutcomeDeparted || rec.DecidedBy != trace.DecidedByJev || rec.Judgment != moved.Judgment || rec.Default != intake.UnitContested || rec.Target != intake.UnitImplementing {
		t.Fatalf("route %+v", rec)
	}
	var span *router.SpanRoute
	for _, s := range log.all() {
		if s.Trigger == "unit.review_rejected" {
			span = &s
		}
	}
	if span == nil || span.Outcome != router.OutcomeDeparted || span.Default != intake.UnitContested || span.Chosen != intake.UnitImplementing || span.Judgment != moved.Judgment {
		t.Fatalf("route span %+v", span)
	}
	f.s.setSole(&activeProject{repository: repo})
	feed, apiErr := service.InspectForTest(f.s.core()).Feed(string(stream))
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
	routeThrough(f.s, p)
	out, apiErr := f.s.ownerMove(ctx, string(stream), "resume", api.UnitMoveRequest{To: intake.UnitApproved, Note: "The owner read the candidate."})
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
	rec := routeOf(t, repo, stream, "unit.move_owner")
	if rec.Authority != router.AuthorityOwner || rec.Outcome != router.OutcomeFollowed || rec.Rule != "pinned" || rec.JevRequested {
		t.Fatalf("route %+v", rec)
	}
	if len(p.Requests()) != 0 {
		t.Fatalf("Jev was asked about an owner's decision: %d requests", len(p.Requests()))
	}
	if _, _, ok, err := (&foreman{masons: newMasonController(f.s, repo)}).approval(stream, "resume", 0); err != nil || !ok {
		t.Fatalf("the owner's approval record: %v %v", ok, err)
	}
}

func TestAChiefMoveToApprovedIsNeverLegal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "route-chief-approval")
	routeThrough(f.s, &jevfixture.Transitions{})
	before := workflowOf(t, repo, stream, "resume")
	x, err := f.s.execution().UnitExit(execution.UnitExitInput{Repo: repo, Stream: stream, Unit: "resume", Trigger: "unit.move_proposal", Cause: "chief-proposal-1",
		Default: intake.UnitApproved, Note: "Looks fine.", Actor: intake.ChiefActor, Active: true})
	fixturekit.Must(t, err)
	res, err := f.s.core().RouteExit(ctx, x)
	fixturekit.Must(t, err)
	if res.Committed || res.Outcome != router.OutcomeHeld || res.Hold != router.HoldGate || res.Journal != router.StateOwner {
		t.Fatalf("the router entered approved on a chief proposal: %+v", res)
	}
	if got := workflowOf(t, repo, stream, "resume"); got != before {
		t.Fatalf("the unit moved to %+v", got)
	}
}
