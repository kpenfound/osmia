package executionjourney

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// blockingFinalReview assembles the sealed plan and records a final review
// that finds one blocking problem, with the router asking p. It returns the
// operation's result.
func blockingFinalReview(t *testing.T, f *shedFixture, repository *trace.Repository, a *finalReviewer, stream config.WorkstreamID) (coreadapter.OperationResult, *spanLog, *jevfixture.Transitions) {
	t.Helper()
	return blockingFinalReviewWith(t, f, repository, a, stream, &jevfixture.Transitions{})
}

func blockingFinalReviewWith(t *testing.T, f *shedFixture, repository *trace.Repository, a *finalReviewer, stream config.WorkstreamID, p *jevfixture.Transitions) (coreadapter.OperationResult, *spanLog, *jevfixture.Transitions) {
	t.Helper()
	log := routeThrough(f.s, p)
	_, op := assembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Dedupe is not wired in.", "findings": []any{
			map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}}})
		return err
	})
	return settleOperation(t, f.s, repository, stream, op, a), log, p
}

// moveDocuments returns the recorded moves of the workstream.
func moveDocuments(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, d := range workstreamDocuments(t, repository, stream) {
		if strings.HasPrefix(d.Path, "moves/move-") {
			var m map[string]any
			must(t, json.Unmarshal([]byte(d.Content), &m))
			out = append(out, m)
		}
	}
	return out
}

func workstreamDocuments(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	return docs
}

func departuresInFeed(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID) []trace.Transition {
	t.Helper()
	f.s.setSole(&activeProject{repository: repository})
	feed, api := service.InspectForTest(f.s.core()).Feed(string(stream))
	if api != nil {
		t.Fatal(api.Message)
	}
	var out []trace.Transition
	for _, e := range feed.Entries {
		if e.Transition != nil && e.Transition.Departed {
			out = append(out, trace.Transition{Subject: e.Transition.Subject, To: e.Transition.To, DefaultTo: e.Transition.DefaultTo, DecidedBy: e.Transition.DecidedBy})
		}
	}
	return out
}

// With the fixture Jev that finds the default fits, a final review's round is
// recorded as it always was, and its transition records the default it took.
func TestAFinalReviewRoundThatFitsItsDefaultRecordsIt(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "route-final-default")
	result, _, p := blockingFinalReview(t, f, repository, a, stream)
	if result.Outcome != "succeeded" {
		t.Fatalf("the review %+v", result)
	}
	moved := latestOf(t, repository, stream, finalReviewSubject)
	if moved.ID != "final-review-1-reviewed" || moved.To != "reviewed-1" || moved.DefaultTo != "reviewed-1" || moved.DecidedBy != trace.DecidedByCode || moved.Departed() || moved.Actor != finalReviewActor {
		t.Fatalf("the round's transition %+v", moved)
	}
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
		t.Fatalf("the workstream %+v %v", feature, err)
	}
	if body := noticeOf(t, repository, stream, "final-review-1-reviewed"); !strings.Contains(body, "1 blocking, 0 advisory.\nBlocking: spec#2: The upload path never calls Dedupe.") {
		t.Fatalf("the notice %q", body)
	}
	rec := routeOf(t, repository, stream, "final.reviewed_blocking")
	if rec.State != router.StateCommitted || rec.Outcome != router.OutcomeFollowed || rec.Default != "reviewed" || rec.Target != "reviewed" {
		t.Fatalf("route %+v", rec)
	}
	if n := len(p.TransitionRequests()); n > 1 {
		t.Fatalf("Jev was asked %d times for one exit", n)
	}
	if got := departuresInFeed(t, f, repository, stream); len(got) != 0 {
		t.Fatalf("a followed default is shown as a departure: %+v", got)
	}
	if moves := moveDocuments(t, repository, stream); len(moves) != 0 {
		t.Fatalf("a followed default moved the workstream: %+v", moves)
	}
}

// A scripted departure after a blocking final review reopens the workstream
// for repair units when it clears the strict bar: the round is recorded, the
// workstream moves to building with a recorded move, and the departure is
// recorded and shown.
func TestAScriptedDepartureReopensTheWorkstreamAfterABlockingFinalReview(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "route-final-reopen")
	p := &jevfixture.Transitions{Script: func(ask jevfixture.Ask) jevfixture.Reply {
		if ask.Trigger == "final.reviewed_blocking" {
			return jevfixture.Departs(ask, "reopen_building", 0.96, 0.96)
		}
		return jevfixture.DefaultFits(ask)
	}}
	result, log, _ := blockingFinalReviewWith(t, f, repository, a, stream, p)
	if result.Outcome != "succeeded" {
		t.Fatalf("the review %+v", result)
	}
	moved := latestOf(t, repository, stream, finalReviewSubject)
	if moved.ID != "final-review-1-reviewed" || moved.To != "reopened-1" || moved.DefaultTo != "reviewed-1" || moved.DecidedBy != trace.DecidedByJev || !moved.Departed() || moved.Judgment == "" {
		t.Fatalf("the round's transition %+v", moved)
	}
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != BuildingState {
		t.Fatalf("the workstream %+v %v", feature, err)
	}
	if body := noticeOf(t, repository, stream, "final-review-1-reviewed"); !strings.Contains(body, "1 blocking") {
		t.Fatalf("the report's notice %q", body)
	}
	moves := moveDocuments(t, repository, stream)
	if len(moves) != 1 || moves[0]["to"] != BuildingState || moves[0]["by"] != "jev" {
		t.Fatalf("the recorded moves %+v", moves)
	}
	rec := routeOf(t, repository, stream, "final.reviewed_blocking")
	if rec.Outcome != router.OutcomeDeparted || rec.DecidedBy != trace.DecidedByJev || rec.Judgment != moved.Judgment || rec.Target != "reopen_building" {
		t.Fatalf("route %+v", rec)
	}
	var span *router.SpanRoute
	for _, s := range log.all() {
		if s.Trigger == "final.reviewed_blocking" {
			span = &s
		}
	}
	if span == nil || span.Outcome != router.OutcomeDeparted || span.Default != "reviewed" || span.Chosen != "reopen_building" || span.Judgment != moved.Judgment {
		t.Fatalf("route span %+v", span)
	}
	if got := departuresInFeed(t, f, repository, stream); len(got) != 1 || got[0].To != "reopened-1" || got[0].DecidedBy != trace.DecidedByJev {
		t.Fatalf("the feed's departures %+v", got)
	}
}

// A departure that does not clear the strict bar commits nothing: the report
// is still recorded as the default records it, the workstream stays
// assembled, and the router holds the exit with one notice for the chief of
// staff.
func TestADepartureBelowItsBarRecordsTheReportAndHolds(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := newFinalFixture(t, "route-final-below-bar")
	p := &jevfixture.Transitions{Script: func(ask jevfixture.Ask) jevfixture.Reply {
		if ask.Trigger == "final.reviewed_blocking" {
			return jevfixture.Departs(ask, "reopen_building", 0.8, 0.96)
		}
		return jevfixture.DefaultFits(ask)
	}}
	result, _, _ := blockingFinalReviewWith(t, f, repository, a, stream, p)
	if result.Outcome != "succeeded" {
		t.Fatalf("the review %+v", result)
	}
	moved := latestOf(t, repository, stream, finalReviewSubject)
	if moved.ID != "final-review-1-reviewed" || moved.To != "reviewed-1" || moved.DecidedBy != "" {
		t.Fatalf("the report is recorded as the default records it: %+v", moved)
	}
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
		t.Fatalf("the workstream %+v %v", feature, err)
	}
	rec := routeOf(t, repository, stream, "final.reviewed_blocking")
	if rec.State != router.StateHeld || rec.Outcome != router.OutcomeHeld || rec.Hold != router.HoldBelowBar {
		t.Fatalf("route %+v", rec)
	}
	notices := 0
	outbox, err := repository.Outbox(stream)
	must(t, err)
	for _, e := range outbox {
		if e.Event.Kind == trace.NoticeKind && strings.HasPrefix(e.TransitionID, "route-") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("%d router notices, want one", notices)
	}
	if moves := moveDocuments(t, repository, stream); len(moves) != 0 {
		t.Fatalf("a held exit moved the workstream: %+v", moves)
	}
}
