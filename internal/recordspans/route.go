package recordspans

import (
	"math"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/trace"
)

// The outcomes of a route, the values of the outcome attribute of an
// osmia.transition.route span: the default was committed (followed), Jev's
// answers committed another legal target (departed), nothing was committed
// and the subject was held (held), or Jev could not answer and the default was
// committed (fallback). A move an owner or a gate decision authorised is
// followed.
const (
	RouteFollowed = "followed"
	RouteDeparted = "departed"
	RouteHeld     = "held"
	RouteFallback = "fallback"
)

// RouteOutcomes lists every outcome a route span can carry.
var RouteOutcomes = []string{RouteFollowed, RouteDeparted, RouteHeld, RouteFallback}

// The confidence bands, the values of the confidence attribute of a route
// span. A band says how sure Jev was of the state the route chose, without
// the probability: none when no answer of Jev's chose it (a pinned move or a
// fallback), low below ConfidenceModest, modest from there to
// ConfidenceHigh, high from there to the decision threshold, and binding at or
// above it.
const (
	ConfidenceNone    = "none"
	ConfidenceLow     = "low"
	ConfidenceModest  = "modest"
	ConfidenceHigh    = "high"
	ConfidenceBinding = "binding"
)

// The probabilities at which a route's confidence rises to the next band;
// binding starts at the decision threshold ConfidenceBand is given.
const (
	ConfidenceModestFrom = 0.5
	ConfidenceHighFrom   = 0.7
)

// ConfidenceBands lists every band a route span can carry.
var ConfidenceBands = []string{ConfidenceNone, ConfidenceLow, ConfidenceModest, ConfidenceHigh, ConfidenceBinding}

// ConfidenceBand returns the band of probability p, given the decision
// threshold that starts binding. A probability that is not a number between
// zero and one has no band.
func ConfidenceBand(p, decisionThreshold float64) string {
	switch {
	case math.IsNaN(p) || p < 0 || p > 1:
		return ConfidenceNone
	case p >= decisionThreshold:
		return ConfidenceBinding
	case p >= ConfidenceHighFrom:
		return ConfidenceHigh
	case p >= ConfidenceModestFrom:
		return ConfidenceModest
	}
	return ConfidenceLow
}

// RouteTriggers lists every trigger a route span can carry: what raised the
// exit, as machine.event. The router raises an exit for a trigger on this
// list and no other.
var RouteTriggers = []string{
	"unit.created", "unit.deps_merged", "unit.mason_start", "unit.question_follow", "unit.question_wait_broken",
	"unit.mason_done", "unit.mason_ended_clean", "unit.markers_left", "unit.turn_failed",
	"unit.checks_passed", "unit.checks_failed", "unit.checks_incomplete",
	"unit.review_approved", "unit.review_rejected", "unit.review_no_verdict", "unit.review_refused", "unit.review_needs_checks",
	"unit.rebased", "unit.rebase_conflicted", "unit.landed", "unit.landing_refused",
	"unit.move_owner", "unit.move_proposal", "unit.stalled",
	"ws.handed", "ws.draft_accepted", "ws.shed_entered", "ws.build_started", "ws.units_merged",
	"ws.draft_exhausted", "ws.shed_concluded", "ws.ratified", "ws.seal_failed",
	"ws.delivery_approved", "ws.ready_on_host", "ws.merge_recorded", "ws.abandon",
	"ws.move_owner", "ws.move_proposal", "ws.stalled",
	"final.requested", "final.reviewed_clean", "final.reviewed_blocking", "final.failed", "final.checks_missing",
	"contest.owner_ruling", "contest.jev_ruling", "contest.chief_ruling", "objection.chief_decision",
	"dep.edge_wait", "dep.edge_resolved", "dep.upstream_terminal", "dep.upstream_integrated", "dep.base_unavailable", "dep.base_available", "dep.base_integrated",
	"drift.requested", "drift.clean", "drift.review_approved", "drift.review_rejected", "drift.turn_failed", "drift.held",
}

// Route describes one exit the transition router decided: the subject it was
// raised for, the trigger, the state the default would have entered and the
// state the route chose, the outcome, the confidence band and the Jev
// judgment behind it. Chosen is the state the subject is in once the exit is
// decided, so for a held exit the state it stays in.
//
// Occurrence identifies the exit and Start and End are the recorded times it
// was raised and decided; the span's identity comes from the workstream and
// Occurrence, so deciding an exit twice sends one span. Unit is empty for a
// workstream exit and Judgment empty when no judgment record is behind the
// decision.
type Route struct {
	Project    config.ProjectID
	Workstream config.WorkstreamID
	Unit       string
	Occurrence string
	Trigger    string
	Default    string
	Chosen     string
	Outcome    string
	Confidence string
	Judgment   string
	Start, End time.Time
}

// Route sends the osmia.transition.route span of one exit and reports whether
// it did. It sends nothing for a workstream-less exit, an occurrence without
// an identity, or a trigger, outcome or confidence band that is not on the
// fixed lists above, so a value that is not a fixed word never reaches a span.
func (r Recorder) Route(route Route) bool {
	if !r.Sink.Enabled() || route.Workstream == "" || route.Occurrence == "" || route.End.IsZero() {
		return false
	}
	if !slices.Contains(RouteTriggers, route.Trigger) || !slices.Contains(RouteOutcomes, route.Outcome) || !slices.Contains(ConfidenceBands, route.Confidence) {
		return false
	}
	start := route.Start
	if start.IsZero() || start.After(route.End) {
		start = route.End
	}
	w := route.Workstream
	attrs := []telemetry.Attr{
		telemetry.Project(route.Project), telemetry.Workstream(w),
		telemetry.Trigger(route.Trigger), telemetry.DefaultState(route.Default), telemetry.NextState(route.Chosen),
		telemetry.Outcome(route.Outcome), telemetry.Confidence(route.Confidence),
	}
	if route.Unit != "" {
		attrs = append(attrs, telemetry.Unit(route.Unit))
	}
	if route.Judgment != "" {
		attrs = append(attrs, telemetry.Judgment(route.Judgment))
	}
	r.Sink.Emit(telemetry.Span{
		Name: telemetry.SpanName("transition", "route"), Trace: telemetry.WorkstreamTrace(w),
		ID:     telemetry.WorkstreamSpan(w, "route", route.Occurrence),
		Parent: telemetry.WorkstreamRoot(w), Start: start, End: route.End,
		Attrs: attrs,
	})
	return true
}

// routing returns the attributes that say how transition t was chosen: who
// decided it, the state its default would have entered, and whether it
// entered another. A transition no router chose has none.
func routing(t trace.Transition) []telemetry.Attr {
	var attrs []telemetry.Attr
	if t.DecidedBy != "" {
		attrs = append(attrs, telemetry.DecidedBy(t.DecidedBy))
	}
	if deviated, known := t.Deviated(); known {
		attrs = append(attrs, telemetry.DefaultState(t.DefaultTo), telemetry.Deviated(deviated))
	}
	return attrs
}
