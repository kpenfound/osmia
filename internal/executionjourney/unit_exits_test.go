package executionjourney

import (
	"context"
	"encoding/json"
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

// spanLog collects the route spans the router sends.
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

// routeThrough makes the service's router ask a Jev that answers as p does, and
// collects the route spans it sends.
func routeThrough(s *Service, p *jevfixture.Transitions) *spanLog {
	service.InspectForTest(s.core()).SetJudge(jevfixture.TransitionJudge(s.now, p))
	log := &spanLog{}
	s.core().Router().Spans = log.add
	return log
}

// routeRecords returns the route journal of the workstream's exits, the latest
// revision of each.
func routeRecords(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []router.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
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
		must(t, json.Unmarshal([]byte(latest[id].Content), &rec))
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

// With the fixture Jev that finds the default fits, a unit's exit commits its
// default as it always has, and the transition records the default it took.
func TestAUnitExitThatFitsItsDefaultCommitsItAndRecordsIt(t *testing.T) {
	t.Parallel()
	f, stream, repo := newChecksFixture(t, "route-default", &fakeChecks{Outcomes: passing()})
	p := &jevfixture.Transitions{}
	log := routeThrough(f.s, p)
	runChecks(t, f.s, repo, stream)

	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != UnitReviewing || moved.DefaultTo != UnitReviewing || moved.DecidedBy != trace.DecidedByCode || moved.Departed() || moved.Judgment == "" {
		t.Fatalf("the unit's move to reviewing %+v", moved)
	}
	if !strings.HasPrefix(moved.ID, trace.UnitSubject("resume")+"-"+UnitReviewing+"-checks-") || strings.Contains(moved.Reason, "decide-transition") {
		t.Fatalf("a default that was followed reads as the controller wrote it: %+v", moved)
	}
	rec := routeOf(t, repo, stream, "unit.checks_passed")
	if rec.State != router.StateCommitted || rec.Outcome != router.OutcomeFollowed || rec.Default != UnitReviewing || rec.Target != UnitReviewing {
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
	if span == nil || span.Outcome != router.OutcomeFollowed || span.Default != UnitReviewing || span.Chosen != UnitReviewing || span.Unit != "resume" {
		t.Fatalf("route span %+v", span)
	}
}

// A satisfactory verdict for the unit's current candidate reaches approved
// through the router, which records the verdict as its authority.
func TestASatisfactoryVerdictApprovesThroughTheRouter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newReviewFixture(t, "route-verdict")
	p := &jevfixture.Transitions{}
	routeThrough(f.s, p)
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state := workflowOf(t, repo, stream, "resume")
	_, identity, err := r.prepareUnitReview(ctx, stream, "resume")
	must(t, err)
	result := UnitReviewResult{Identity: identity, Turn: reviewTurnID("resume", state.Version), Verdict: UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}}
	recordVerdict(t, repo, stream, result)

	must(t, r.applyReview(ctx, stream, "resume", state, result))

	if got := workflowOf(t, repo, stream, "resume"); got.Value != UnitApproved {
		t.Fatalf("the unit is %s", got.Value)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != UnitApproved || moved.DecidedBy != trace.DecidedByReviewer || moved.DefaultTo != UnitApproved || moved.Departed() {
		t.Fatalf("the move to approved %+v", moved)
	}
	rec := routeOf(t, repo, stream, "unit.review_approved")
	if rec.Authority != router.AuthorityReviewer || !strings.HasPrefix(rec.AuthorityRef, reviewDocument("resume")+" revision ") || rec.State != router.StateCommitted {
		t.Fatalf("the approval's authority is not the reviewer's verdict: %+v", rec)
	}
	if _, _, ok, err := (&foreman{masons: r.masons}).approval(stream, "resume", 0); err != nil || !ok {
		t.Fatalf("the approval record the foreman lands: %v %v", ok, err)
	}
}

// A verdict that a later review replaced does not approve the unit, whichever
// way the router is asked: approved is not offered, and the unit is held with
// one notice.
func TestAVerdictForASupersededCandidateDoesNotApprove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newReviewFixture(t, "route-superseded")
	routeThrough(f.s, &jevfixture.Transitions{})
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state := workflowOf(t, repo, stream, "resume")
	_, identity, err := r.prepareUnitReview(ctx, stream, "resume")
	must(t, err)
	old := UnitReviewResult{Identity: identity, Turn: reviewTurnID("resume", state.Version), Verdict: UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}}
	recordVerdict(t, repo, stream, old)
	// A later review of the unit replaces the verdict before it is applied.
	newer := UnitReviewResult{Identity: identity, Turn: reviewTurnID("resume", state.Version) + "-recover-1", Verdict: UnitVerdict{Decision: "material_findings", Summary: reviewSummary,
		Findings: []ReviewFinding{{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}}}, Bounces: 1}
	recordVerdict(t, repo, stream, newer)

	must(t, r.applyReview(ctx, stream, "resume", state, old))

	if got := workflowOf(t, repo, stream, "resume"); got.Value != UnitReviewing || got.Version != state.Version {
		t.Fatalf("a superseded verdict moved the unit to %+v", got)
	}
	rec := routeOf(t, repo, stream, "unit.review_approved")
	if rec.State == router.StateCommitted || rec.Outcome != router.OutcomeHeld || rec.Hold != router.HoldDefaultIllegal || !strings.Contains(rec.Reason, "later review") {
		t.Fatalf("route %+v", rec)
	}
	notices := 0
	outbox, err := repo.Outbox(stream)
	must(t, err)
	for _, e := range outbox {
		if e.Event.Kind == trace.NoticeKind && strings.HasPrefix(e.TransitionID, "route-") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("%d notices for the held exit, want one", notices)
	}
}

// recordVerdict records result as the next revision of the unit's review.
func recordVerdict(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, result UnitReviewResult) {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	var latest trace.Document
	for _, d := range docs {
		if d.ID == reviewDocument("resume") {
			latest = d
		}
	}
	data, err := json.MarshalIndent(result, "", "  ")
	must(t, err)
	latest.Revision++
	latest.Content = string(data) + "\n"
	latest.Cause = result.Turn
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{latest}))
}

// A scripted departure from a unit exit: the review's send-backs reached their
// bound, so the default is to contest the unit, and Jev sends the work back to
// the mason instead. The departed state commits with its effects, the mason's
// fresh allowance among them, and the departure is recorded, spanned, shown in
// the feed and nowhere else.
func TestAScriptedDepartureSendsTheWorkBackToTheMasonInsteadOfContesting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newReviewFixture(t, "route-departure")
	p := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		return jevfixture.Departs(a, UnitImplementing, 0.95, 0.95)
	}}
	log := routeThrough(f.s, p)
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state := workflowOf(t, repo, stream, "resume")
	_, identity, err := r.prepareUnitReview(ctx, stream, "resume")
	must(t, err)
	reported := lastTurn(t, repo, stream, masonAgent("resume"))
	findings := []ReviewFinding{{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}}
	result := UnitReviewResult{Identity: identity, Turn: reviewTurnID("resume", state.Version), Bounces: f.s.cfg.Shed.MaxBounces, Verdict: UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: findings}}

	must(t, r.applyReview(ctx, stream, "resume", state, result))

	if got := workflowOf(t, repo, stream, "resume"); got.Value != UnitImplementing {
		t.Fatalf("the unit is %s, want implementing", got.Value)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != UnitImplementing || moved.DefaultTo != UnitContested || moved.DecidedBy != trace.DecidedByJev || moved.Judgment == "" || !moved.Departed() {
		t.Fatalf("the departure %+v", moved)
	}
	if !isMove(moved, "resume") || !strings.Contains(moved.Reason, "decide-transition table") {
		t.Fatalf("the departure is entered through the shared effects of a move: %+v", moved)
	}
	ruling, found, err := latestMasonRuling(repo, stream, "resume")
	must(t, err)
	if !found || ruling.ResetTurn != reported.Sequence {
		t.Fatalf("the mason's fresh allowance %+v", ruling)
	}
	rec := routeOf(t, repo, stream, "unit.review_rejected")
	if rec.Outcome != router.OutcomeDeparted || rec.DecidedBy != trace.DecidedByJev || rec.Judgment != moved.Judgment || rec.Default != UnitContested || rec.Target != UnitImplementing {
		t.Fatalf("route %+v", rec)
	}
	var span *router.SpanRoute
	for _, s := range log.all() {
		if s.Trigger == "unit.review_rejected" {
			span = &s
		}
	}
	if span == nil || span.Outcome != router.OutcomeDeparted || span.Default != UnitContested || span.Chosen != UnitImplementing || span.Judgment != moved.Judgment {
		t.Fatalf("route span %+v", span)
	}
	f.s.setSole(&activeProject{repository: repo})
	feed, api := service.InspectForTest(f.s.core()).Feed(string(stream))
	if api != nil {
		t.Fatal(api.Message)
	}
	departed := 0
	for _, e := range feed.Entries {
		if e.Transition != nil && e.Transition.Departed {
			departed++
			if e.Transition.DecidedBy != trace.DecidedByJev || e.Transition.DefaultTo != UnitContested || e.Transition.To != UnitImplementing {
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

// An owner approving a unit without independent review is a pinned move: it
// commits exactly approved with the approval record, names the owner as the
// decider, and asks Jev nothing.
func TestAnOwnerApprovalIsAPinnedMove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "route-owner-approval")
	p := &jevfixture.Transitions{}
	routeThrough(f.s, p)
	out, api := f.s.ownerMove(ctx, string(stream), "resume", UnitMoveRequest{To: UnitApproved, Note: "The owner read the candidate."})
	if api != nil {
		t.Fatal(api.Message)
	}
	if out.Move.To != UnitApproved || out.Move.By != "owner" {
		t.Fatalf("move %+v", out.Move)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != UnitApproved || moved.DecidedBy != trace.DecidedByOwner || moved.DefaultTo != UnitApproved || moved.Departed() {
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

// A move of the chief of staff's own is no owner decision: approved is never
// offered on it, so the router holds it and the unit does not move.
func TestAChiefMoveToApprovedIsNeverLegal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newCheckingFixture(t, "route-chief-approval")
	routeThrough(f.s, &jevfixture.Transitions{})
	before := workflowOf(t, repo, stream, "resume")
	x, err := f.s.execution().UnitExit(execution.UnitExitInput{Repo: repo, Stream: stream, Unit: "resume", Trigger: "unit.move_proposal", Cause: "chief-proposal-1",
		Default: UnitApproved, Note: "Looks fine.", Actor: chiefActor, Active: true})
	must(t, err)
	res, err := f.s.core().RouteExit(ctx, x)
	must(t, err)
	if res.Committed || res.Outcome != router.OutcomeHeld || res.Hold != router.HoldDefaultIllegal {
		t.Fatalf("the router entered approved on a chief proposal: %+v", res)
	}
	if got := workflowOf(t, repo, stream, "resume"); got != before {
		t.Fatalf("the unit moved to %+v", got)
	}
}
