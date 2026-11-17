package executionjourney

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// lastRouteOf returns the route record of the latest exit with the trigger.
func lastRouteOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, trigger string) router.Record {
	t.Helper()
	var out *router.Record
	for _, rec := range routeRecords(t, repo, stream) {
		if rec.Trigger == trigger {
			rec := rec
			out = &rec
		}
	}
	if out == nil {
		t.Fatalf("no exit of trigger %s was routed", trigger)
	}
	return *out
}

// pickingTheRetry scripts a Jev that picks target with every question
// corroborating it whenever target is on offer, and finds the default fits
// otherwise. seen collects every option Jev was offered.
func pickingTheRetry(target string, seen *[]string) func(jevfixture.Ask) jevfixture.Reply {
	var mu sync.Mutex
	return func(a jevfixture.Ask) jevfixture.Reply {
		mu.Lock()
		*seen = append(*seen, a.Options...)
		mu.Unlock()
		if slices.Contains(a.Options, target) {
			return jevfixture.Departs(a, target, 0.99, 0.99)
		}
		return jevfixture.DefaultFits(a)
	}
}

// The review's send-back count reaching shed.max_bounces sets the default of a
// rejected review to contested, with the mason's turn and a second review still
// on offer. With a Jev that finds the default fits the unit is contested as it
// always was, and the transition records the default it took. The departure
// past the limit is TestAScriptedDepartureSendsTheWorkBackToTheMasonInsteadOfContesting.
func TestTheReviewSendBackCountSetsTheDefaultOfARejectedReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := newReviewFixture(t, "send-back-default")
	p := &jevfixture.Transitions{}
	routeThrough(f.s, p)
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state := workflowOf(t, repo, stream, "resume")
	_, identity, err := r.prepareUnitReview(ctx, stream, "resume")
	must(t, err)
	findings := []ReviewFinding{{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}}
	result := UnitReviewResult{Identity: identity, Turn: reviewTurnID("resume", state.Version), Bounces: f.s.cfg.Shed.MaxBounces, Verdict: UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: findings}}

	must(t, r.applyReview(ctx, stream, "resume", state, result))

	if got := workflowOf(t, repo, stream, "resume"); got.Value != UnitContested {
		t.Fatalf("the unit is %s, want contested", got.Value)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != UnitContested || moved.DefaultTo != UnitContested || moved.DecidedBy != trace.DecidedByCode || moved.Departed() || moved.Judgment == "" {
		t.Fatalf("the transition %+v", moved)
	}
	rec := routeOf(t, repo, stream, "unit.review_rejected")
	for _, offered := range []string{UnitImplementing, UnitReviewing, UnitContested} {
		if !slices.Contains(rec.Offered, offered) {
			t.Fatalf("the limit left only %v on offer", rec.Offered)
		}
	}
	if rec.Outcome != router.OutcomeFollowed || rec.Default != UnitContested || rec.Target != UnitContested {
		t.Fatalf("route %+v", rec)
	}
}

// maxIncompleteChecks stays a hard limit: once the checks of a candidate did not
// complete that many times in a row the unit is contested and running them again
// is not offered, however Jev would answer.
func TestMaxIncompleteChecksOffersNoRetry(t *testing.T) {
	t.Parallel()
	var outcomes []checkOutcome
	for range maxIncompleteChecks {
		outcomes = append(outcomes, checkOutcome{Err: errors.New("engine unreachable")})
	}
	checks := &fakeChecks{Outcomes: append(outcomes, checkOutcome{Result: CheckResult{Output: passedReport}})}
	f, stream, repository := newChecksFixture(t, "hard-incomplete", checks)
	var seen []string
	p := &jevfixture.Transitions{Script: pickingTheRetry(UnitChecking, &seen)}
	routeThrough(f.s, p)
	for range maxIncompleteChecks {
		runChecks(t, f.s, repository, stream)
		f.clock.Mu.Lock()
		f.clock.At = f.clock.At.Add(checkRetryDelay + time.Minute)
		f.clock.Mu.Unlock()
	}
	runChecks(t, f.s, repository, stream)

	if got := unitState(t, repository, stream, "resume"); got != UnitContested {
		t.Fatalf("the unit is %s after %d incomplete runs", got, maxIncompleteChecks)
	}
	rec := lastRouteOf(t, repository, stream, "unit.checks_incomplete")
	if !slices.Equal(rec.Offered, []string{UnitContested}) || rec.Outcome != router.OutcomeFollowed || rec.Target != UnitContested {
		t.Fatalf("route %+v", rec)
	}
	if len(seen) != 0 || len(p.TransitionRequests()) != 0 {
		t.Fatalf("Jev was asked about a hard limit with one legal target: %v", seen)
	}
	if ran := checks.Runs(); len(ran) != maxIncompleteChecks {
		t.Fatalf("checks ran %d times", len(ran))
	}
}

// maxReviewRefreshes stays a hard limit: the review that would be the
// maxReviewRefreshes-th refused in a row contests the unit, and reviewing again
// is not offered, while a fresh check run still is.
func TestMaxReviewRefreshesOffersNoSecondReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := newReviewFixture(t, "hard-refresh")
	var seen []string
	routeThrough(f.s, &jevfixture.Transitions{Script: pickingTheRetry(UnitReviewing, &seen)})
	r := &reviewers{masons: newMasonController(f.s, repository)}
	for i := 1; i <= maxReviewRefreshes; i++ {
		state := workflowOf(t, repository, stream, "resume")
		must(t, r.refreshReview(ctx, stream, "resume", state, "stale base revision; review the current candidate again"))
	}

	if got := workflowOf(t, repository, stream, "resume"); got.Value != UnitContested {
		t.Fatalf("the unit is %s after %d refused reviews", got.Value, maxReviewRefreshes)
	}
	rec := lastRouteOf(t, repository, stream, "unit.review_refused")
	if slices.Contains(rec.Offered, UnitReviewing) || !slices.Contains(rec.Offered, UnitChecking) || !slices.Contains(rec.Offered, UnitContested) || rec.Default != UnitContested || rec.Target != UnitContested {
		t.Fatalf("route %+v", rec)
	}
	moved := latestOf(t, repository, stream, trace.UnitSubject("resume"))
	if moved.To != UnitContested || moved.Departed() {
		t.Fatalf("the transition %+v", moved)
	}
}

// maxRecoveries stays a hard limit: once that many turns of the unit's reviewer
// were interrupted in a row the unit is contested and a new review turn is not
// offered.
func TestMaxRecoveriesOffersNoRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := newReviewFixture(t, "hard-recoveries")
	var seen []string
	p := &jevfixture.Transitions{Script: pickingTheRetry(UnitReviewing, &seen)}
	routeThrough(f.s, p)
	r := &reviewers{masons: newMasonController(f.s, repository)}
	subject := trace.UnitSubject("resume")
	state, err := repository.Workflow(stream, subject)
	must(t, err)
	must(t, r.one(ctx, stream, "resume", state, false))
	for range maxRecoveries {
		interruptTurn(t, f, repository, stream, reviewerAgent("resume"))
		must(t, r.one(ctx, stream, "resume", state, false))
	}

	if got, err := repository.Workflow(stream, subject); err != nil || got.Value != UnitContested {
		t.Fatalf("the unit is %+v after %d interruptions: %v", got, maxRecoveries, err)
	}
	rec := lastRouteOf(t, repository, stream, "unit.turn_failed")
	if !slices.Equal(rec.Offered, []string{UnitContested}) || rec.Outcome != router.OutcomeFollowed || strings.Contains(strings.Join(seen, ","), UnitReviewing) {
		t.Fatalf("route %+v, options seen %v", rec, seen)
	}
}

// pastTheFinalRoundLimit assembles the sealed plan, records maxFinalRounds final
// reviews that found something blocking, and records one more blocking final
// review with the router asking p. It returns the operation's result.
func pastTheFinalRoundLimit(t *testing.T, f *shedFixture, repository *trace.Repository, a *finalReviewer, stream config.WorkstreamID, p *jevfixture.Transitions) coreadapter.OperationResult {
	t.Helper()
	routeThrough(f.s, p)
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	for review := 1; review <= maxFinalRounds; review++ {
		content, err := json.Marshal(FinalReport{Review: review, Outcome: finalReviewed, Findings: []FinalFinding{{Severity: FindingBlocking, Finding: "The upload path never calls Dedupe."}}})
		must(t, err)
		must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalReportDocument, Revision: review, Project: repository.Project(), Workstream: stream, At: f.s.now(), Actor: finalReviewActor, Cause: "final-review"}, Path: finalReportPath, Content: string(content)}}))
	}
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Dedupe is not wired in.", "findings": []any{
			map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}}})
		return err
	})
	return settleOperation(t, f.s, repository, stream, op, a)
}

// maxFinalRounds sets the default of a blocking final review: once that many
// reviews found something blocking, the default leaves the findings to the owner
// as it always did. With a Jev that finds the default fits, behaviour is the
// same; a scripted departure reopens the workstream past the limit, and records
// the departure with its reason.
func TestTheFinalRoundLimitSetsTheDefaultOfABlockingFinalReview(t *testing.T) {
	t.Parallel()
	t.Run("the default fits", func(t *testing.T) {
		t.Parallel()
		f, stream, repository, a := newFinalFixture(t, "final-limit-default")
		result := pastTheFinalRoundLimit(t, f, repository, a, stream, &jevfixture.Transitions{})
		if result.Outcome != "succeeded" {
			t.Fatalf("the review %+v", result)
		}
		moved := latestOf(t, repository, stream, finalReviewSubject)
		if moved.To != "reviewed-1" || moved.DefaultTo != "reviewed-1" || moved.DecidedBy != trace.DecidedByCode || moved.Departed() {
			t.Fatalf("the round's transition %+v", moved)
		}
		if body := noticeOf(t, repository, stream, "final-review-1-reviewed"); !strings.Contains(body, "Further revisions of the assembled plan are the owner's decision") {
			t.Fatalf("the notice %q", body)
		}
		rec := routeOf(t, repository, stream, "final.reviewed_blocking")
		if rec.Outcome != router.OutcomeFollowed || rec.Default != "reviewed" || !slices.Contains(rec.Offered, "reopen_building") {
			t.Fatalf("the limit did not leave the reopening on offer: %+v", rec)
		}
		if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
			t.Fatalf("the workstream %+v %v", feature, err)
		}
	})

	t.Run("a scripted departure", func(t *testing.T) {
		t.Parallel()
		f, stream, repository, a := newFinalFixture(t, "final-limit-departure")
		p := &jevfixture.Transitions{Script: func(ask jevfixture.Ask) jevfixture.Reply {
			if ask.Trigger == "final.reviewed_blocking" {
				return jevfixture.Departs(ask, "reopen_building", 0.96, 0.96)
			}
			return jevfixture.DefaultFits(ask)
		}}
		result := pastTheFinalRoundLimit(t, f, repository, a, stream, p)
		if result.Outcome != "succeeded" {
			t.Fatalf("the review %+v", result)
		}
		moved := latestOf(t, repository, stream, finalReviewSubject)
		if moved.To != "reopened-1" || moved.DefaultTo != "reviewed-1" || moved.DecidedBy != trace.DecidedByJev || !moved.Departed() || moved.Judgment == "" ||
			!strings.Contains(moved.Reason, "decide-transition table "+router.TableVersion+", rule 6:departure") {
			t.Fatalf("the round's transition %+v", moved)
		}
		if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != BuildingState {
			t.Fatalf("the workstream %+v %v", feature, err)
		}
		rec := routeOf(t, repository, stream, "final.reviewed_blocking")
		if rec.Outcome != router.OutcomeDeparted || rec.Judgment != moved.Judgment || rec.Reason == "" || rec.Target != "reopen_building" {
			t.Fatalf("route %+v", rec)
		}
		if moves := moveDocuments(t, repository, stream); len(moves) != 1 || moves[0]["to"] != BuildingState || moves[0]["by"] != "jev" {
			t.Fatalf("the recorded moves %+v", moves)
		}
	})
}
