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

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func lastRouteOf(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, trigger string) router.Record {
	t.Helper()
	var out *router.Record
	for _, rec := range journey.RouteRecords(t, repo, stream) {
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

func TestTheReviewSendBackCountSetsTheDefaultOfARejectedReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repo := journey.NewReviewFixture(t, "send-back-default")
	p := &jevfixture.Transitions{}
	journey.RouteThrough(f.S, p)
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	state := intakefixture.UnitWorkflow(t, repo, stream, "resume")
	_, identity, err := r.PrepareUnitReview(ctx, stream, "resume")
	fixturekit.Must(t, err)
	findings := []api.ReviewFinding{{Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}}
	result := service.UnitReviewResult{Identity: identity, Turn: execution.ReviewTurnID("resume", state.Version), Bounces: f.S.Cfg.Shed.MaxBounces, Verdict: service.UnitVerdict{Decision: "material_findings", Summary: fixturekit.ReviewSummary, Findings: findings}}

	fixturekit.Must(t, r.Driver().ApplyReview(ctx, stream, "resume", state, result))

	if got := intakefixture.UnitWorkflow(t, repo, stream, "resume"); got.Value != intake.UnitContested {
		t.Fatalf("the unit is %s, want contested", got.Value)
	}
	moved := latestOf(t, repo, stream, trace.UnitSubject("resume"))
	if moved.To != intake.UnitContested || moved.DefaultTo != intake.UnitContested || moved.DecidedBy != trace.DecidedByCode || moved.Departed() || moved.Judgment == "" {
		t.Fatalf("the transition %+v", moved)
	}
	rec := journey.RouteOf(t, repo, stream, "unit.review_rejected")
	for _, offered := range []string{intake.UnitImplementing, intake.UnitReviewing, intake.UnitContested} {
		if !slices.Contains(rec.Offered, offered) {
			t.Fatalf("the limit left only %v on offer", rec.Offered)
		}
	}
	if rec.Outcome != router.OutcomeFollowed || rec.Default != intake.UnitContested || rec.Target != intake.UnitContested {
		t.Fatalf("route %+v", rec)
	}
}

func TestMaxIncompleteChecksOffersNoRetry(t *testing.T) {
	t.Parallel()
	var outcomes []fixturekit.CheckOutcome
	for range execution.MaxIncompleteChecks {
		outcomes = append(outcomes, fixturekit.CheckOutcome{Err: errors.New("engine unreachable")})
	}
	checker := &fixturekit.FakeChecks{Outcomes: append(outcomes, fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}})}
	f, stream, repository := newChecksFixture(t, "hard-incomplete", checker)
	var seen []string
	p := &jevfixture.Transitions{Script: pickingTheRetry(execution.UnitChecking, &seen)}
	journey.RouteThrough(f.S, p)
	for range execution.MaxIncompleteChecks {
		journey.RunChecks(t, f.S, repository, stream)
		f.Clock.Mu.Lock()
		f.Clock.At = f.Clock.At.Add(execution.CheckRetryDelay + time.Minute)
		f.Clock.Mu.Unlock()
	}
	journey.RunChecks(t, f.S, repository, stream)

	if got := intakefixture.UnitState(t, repository, stream, "resume"); got != intake.UnitContested {
		t.Fatalf("the unit is %s after %d incomplete runs", got, execution.MaxIncompleteChecks)
	}
	rec := lastRouteOf(t, repository, stream, "unit.checks_incomplete")
	if !slices.Equal(rec.Offered, []string{intake.UnitContested}) || rec.Outcome != router.OutcomeFollowed || rec.Target != intake.UnitContested {
		t.Fatalf("route %+v", rec)
	}
	if len(seen) != 0 || len(p.TransitionRequests()) != 0 {
		t.Fatalf("Jev was asked about a hard limit with one legal target: %v", seen)
	}
	if ran := checker.Runs(); len(ran) != execution.MaxIncompleteChecks {
		t.Fatalf("checks ran %d times", len(ran))
	}
}

func TestMaxReviewRefreshesOffersNoSecondReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := journey.NewReviewFixture(t, "hard-refresh")
	var seen []string
	journey.RouteThrough(f.S, &jevfixture.Transitions{Script: pickingTheRetry(intake.UnitReviewing, &seen)})
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repository)}
	for i := 1; i <= execution.MaxReviewRefreshes; i++ {
		state := intakefixture.UnitWorkflow(t, repository, stream, "resume")
		fixturekit.Must(t, r.Driver().RefreshReview(ctx, stream, "resume", state, ("stale base revision; review the current candidate again")))
	}

	if got := intakefixture.UnitWorkflow(t, repository, stream, "resume"); got.Value != intake.UnitContested {
		t.Fatalf("the unit is %s after %d refused reviews", got.Value, execution.MaxReviewRefreshes)
	}
	rec := lastRouteOf(t, repository, stream, "unit.review_refused")
	if slices.Contains(rec.Offered, intake.UnitReviewing) || !slices.Contains(rec.Offered, execution.UnitChecking) || !slices.Contains(rec.Offered, intake.UnitContested) || rec.Default != intake.UnitContested || rec.Target != intake.UnitContested {
		t.Fatalf("route %+v", rec)
	}
	moved := latestOf(t, repository, stream, trace.UnitSubject("resume"))
	if moved.To != intake.UnitContested || moved.Departed() {
		t.Fatalf("the transition %+v", moved)
	}
}

func TestMaxRecoveriesOffersNoRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream, repository := journey.NewReviewFixture(t, "hard-recoveries")
	var seen []string
	p := &jevfixture.Transitions{Script: pickingTheRetry(intake.UnitReviewing, &seen)}
	journey.RouteThrough(f.S, p)
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repository)}
	subject := trace.UnitSubject("resume")
	state, err := repository.Workflow(stream, subject)
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.One(ctx, stream, "resume", state, false))
	for range execution.MaxRecoveries {
		interruptTurn(t, f, repository, stream, intake.ReviewerAgent("resume"))
		fixturekit.Must(t, r.One(ctx, stream, "resume", state, false))
	}

	if got, err := repository.Workflow(stream, subject); err != nil || got.Value != intake.UnitContested {
		t.Fatalf("the unit is %+v after %d interruptions: %v", got, execution.MaxRecoveries, err)
	}
	rec := lastRouteOf(t, repository, stream, "unit.turn_failed")
	if !slices.Equal(rec.Offered, []string{intake.UnitContested}) || rec.Outcome != router.OutcomeFollowed || strings.Contains(strings.Join(seen, ","), intake.UnitReviewing) {
		t.Fatalf("route %+v, options seen %v", rec, seen)
	}
}

func pastTheFinalRoundLimit(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, a *journey.FinalReviewer, stream config.WorkstreamID, p *jevfixture.Transitions) coreadapter.OperationResult {
	t.Helper()
	journey.RouteThrough(f.S, p)
	_, op := journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	for review := 1; review <= intake.MaxFinalRounds; review++ {
		content, err := json.Marshal(api.FinalReport{Review: review, Outcome: delivery.FinalReviewed, Findings: []api.FinalFinding{{Severity: delivery.FindingBlocking, Finding: "The upload path never calls Dedupe."}}})
		fixturekit.Must(t, err)
		fixturekit.Must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalReportDocument, Revision: review, Project: repository.Project(), Workstream: stream, At: f.S.Now(), Actor: delivery.FinalReviewActor, Cause: "final-review"}, Path: delivery.FinalReportPath, Content: string(content)}}))
	}
	f.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Dedupe is not wired in.", "findings": []any{
			map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}}})
		return err
	})
	return journey.SettleOperation(t, f.S, repository, stream, op, a)
}

func TestTheFinalRoundLimitSetsTheDefaultOfABlockingFinalReview(t *testing.T) {
	t.Parallel()
	t.Run("the default fits", func(t *testing.T) {
		t.Parallel()
		f, stream, repository, a := journey.NewFinalFixture(t, "final-limit-default")
		result := pastTheFinalRoundLimit(t, f, repository, a, stream, &jevfixture.Transitions{})
		if result.Outcome != "succeeded" {
			t.Fatalf("the review %+v", result)
		}
		moved := latestOf(t, repository, stream, delivery.FinalReviewSubject)
		if moved.To != "reviewed-1" || moved.DefaultTo != "reviewed-1" || moved.DecidedBy != trace.DecidedByCode || moved.Departed() {
			t.Fatalf("the round's transition %+v", moved)
		}
		if body := intakefixture.NoticeOf(t, repository, stream, "final-review-1-reviewed"); !strings.Contains(body, "Further revisions of the assembled plan are the owner's decision") {
			t.Fatalf("the notice %q", body)
		}
		rec := journey.RouteOf(t, repository, stream, "final.reviewed_blocking")
		if rec.Outcome != router.OutcomeFollowed || rec.Default != "reviewed" || !slices.Contains(rec.Offered, "reopen_building") {
			t.Fatalf("the limit did not leave the reopening on offer: %+v", rec)
		}
		if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.AssembledState {
			t.Fatalf("the workstream %+v %v", feature, err)
		}
	})

	t.Run("a scripted departure", func(t *testing.T) {
		t.Parallel()
		f, stream, repository, a := journey.NewFinalFixture(t, "final-limit-departure")
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
		moved := latestOf(t, repository, stream, delivery.FinalReviewSubject)
		if moved.To != "reopened-1" || moved.DefaultTo != "reviewed-1" || moved.DecidedBy != trace.DecidedByJev || !moved.Departed() || moved.Judgment == "" ||
			!strings.Contains(moved.Reason, "decide-transition table "+router.TableVersion+", rule 6:departure") {
			t.Fatalf("the round's transition %+v", moved)
		}
		if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.BuildingState {
			t.Fatalf("the workstream %+v %v", feature, err)
		}
		rec := journey.RouteOf(t, repository, stream, "final.reviewed_blocking")
		if rec.Outcome != router.OutcomeDeparted || rec.Judgment != moved.Judgment || rec.Reason == "" || rec.Target != "reopen_building" {
			t.Fatalf("route %+v", rec)
		}
		if moves := moveDocuments(t, repository, stream); len(moves) != 1 || moves[0]["to"] != intake.BuildingState || moves[0]["by"] != "jev" {
			t.Fatalf("the recorded moves %+v", moves)
		}
	})
}
