package executionjourney

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func blockingFinalReview(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, a *journey.FinalReviewer, stream config.WorkstreamID) (coreadapter.OperationResult, *journey.SpanLog, *jevfixture.Transitions) {
	t.Helper()
	return blockingFinalReviewWith(t, f, repository, a, stream, &jevfixture.Transitions{})
}

func blockingFinalReviewWith(t *testing.T, f *journey.ShedFixture, repository *trace.Repository, a *journey.FinalReviewer, stream config.WorkstreamID, p *jevfixture.Transitions) (coreadapter.OperationResult, *journey.SpanLog, *jevfixture.Transitions) {
	t.Helper()
	log := journey.RouteThrough(f.S, p)
	_, op := journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	f.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Dedupe is not wired in.", "findings": []any{
			map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}}})
		return err
	})
	return journey.SettleOperation(t, f.S, repository, stream, op, a), log, p
}

func moveDocuments(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, d := range workstreamDocuments(t, repository, stream) {
		if strings.HasPrefix(d.Path, "moves/move-") {
			var m map[string]any
			fixturekit.Must(t, json.Unmarshal([]byte(d.Content), &m))
			out = append(out, m)
		}
	}
	return out
}

func workstreamDocuments(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	fixturekit.Must(t, err)
	return docs
}

func TestAFinalReviewRoundThatFitsItsDefaultRecordsIt(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixture(t, "route-final-default")
	result, _, p := blockingFinalReview(t, f, repository, a, stream)
	if result.Outcome != "succeeded" {
		t.Fatalf("the review %+v", result)
	}
	moved := latestOf(t, repository, stream, delivery.FinalReviewSubject)
	if moved.ID != "final-review-1-reviewed" || moved.To != "reviewed-1" || moved.DefaultTo != "reviewed-1" || moved.DecidedBy != trace.DecidedByCode || moved.Departed() || moved.Actor != delivery.FinalReviewActor {
		t.Fatalf("the round's transition %+v", moved)
	}
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.AssembledState {
		t.Fatalf("the workstream %+v %v", feature, err)
	}
	if body := intakefixture.NoticeOf(t, repository, stream, "final-review-1-reviewed"); !strings.Contains(body, "1 blocking, 0 advisory.\nBlocking: spec#2: The upload path never calls Dedupe.") {
		t.Fatalf("the notice %q", body)
	}
	rec := journey.RouteOf(t, repository, stream, "final.reviewed_blocking")
	if rec.State != router.StateCommitted || rec.Outcome != router.OutcomeFollowed || rec.Default != "reviewed" || rec.Target != "reviewed" {
		t.Fatalf("route %+v", rec)
	}
	if n := len(p.TransitionRequests()); n > 1 {
		t.Fatalf("Jev was asked %d times for one exit", n)
	}
	if got := journey.DeparturesInFeed(t, f.S, repository, stream); len(got) != 0 {
		t.Fatalf("a followed default is shown as a departure: %+v", got)
	}
	if moves := moveDocuments(t, repository, stream); len(moves) != 0 {
		t.Fatalf("a followed default moved the workstream: %+v", moves)
	}
}

func TestAScriptedDepartureReopensTheWorkstreamAfterABlockingFinalReview(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixture(t, "route-final-reopen")
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
	moved := latestOf(t, repository, stream, delivery.FinalReviewSubject)
	if moved.ID != "final-review-1-reviewed" || moved.To != "reopened-1" || moved.DefaultTo != "reviewed-1" || moved.DecidedBy != trace.DecidedByJev || !moved.Departed() || moved.Judgment == "" {
		t.Fatalf("the round's transition %+v", moved)
	}
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.BuildingState {
		t.Fatalf("the workstream %+v %v", feature, err)
	}
	if body := intakefixture.NoticeOf(t, repository, stream, "final-review-1-reviewed"); !strings.Contains(body, "1 blocking") {
		t.Fatalf("the report's notice %q", body)
	}
	moves := moveDocuments(t, repository, stream)
	if len(moves) != 1 || moves[0]["to"] != intake.BuildingState || moves[0]["by"] != "jev" {
		t.Fatalf("the recorded moves %+v", moves)
	}
	rec := journey.RouteOf(t, repository, stream, "final.reviewed_blocking")
	if rec.Outcome != router.OutcomeDeparted || rec.DecidedBy != trace.DecidedByJev || rec.Judgment != moved.Judgment || rec.Target != "reopen_building" {
		t.Fatalf("route %+v", rec)
	}
	var span *router.SpanRoute
	for _, s := range log.All() {
		if s.Trigger == "final.reviewed_blocking" {
			span = &s
		}
	}
	if span == nil || span.Outcome != router.OutcomeDeparted || span.Default != "reviewed" || span.Chosen != "reopen_building" || span.Judgment != moved.Judgment {
		t.Fatalf("route span %+v", span)
	}
	if got := journey.DeparturesInFeed(t, f.S, repository, stream); len(got) != 1 || got[0].To != "reopened-1" || got[0].DecidedBy != trace.DecidedByJev {
		t.Fatalf("the feed's departures %+v", got)
	}
}

func TestADepartureBelowItsBarRecordsTheReportAndHolds(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixture(t, "route-final-below-bar")
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
	moved := latestOf(t, repository, stream, delivery.FinalReviewSubject)
	if moved.ID != "final-review-1-reviewed" || moved.To != "reviewed-1" || moved.DecidedBy != "" {
		t.Fatalf("the report is recorded as the default records it: %+v", moved)
	}
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.AssembledState {
		t.Fatalf("the workstream %+v %v", feature, err)
	}
	rec := journey.RouteOf(t, repository, stream, "final.reviewed_blocking")
	if rec.State != router.StateHeld || rec.Outcome != router.OutcomeHeld || rec.Hold != router.HoldBelowBar {
		t.Fatalf("route %+v", rec)
	}
	notices := 0
	outbox, err := repository.Outbox(stream)
	fixturekit.Must(t, err)
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
