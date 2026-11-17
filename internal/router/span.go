package router

import (
	"math"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// The outcomes of a routed exit, the outcome of its route span: the default was
// committed (followed), Jev's answers committed another legal target
// (departed), nothing was committed and the subject was held (held), or Jev
// could not answer and the default was committed (fallback). A move an owner or
// a gate decision authorised is followed.
const (
	OutcomeFollowed = "followed"
	OutcomeDeparted = "departed"
	OutcomeHeld     = "held"
	OutcomeFallback = "fallback"
)

// The confidence bands of a route span: none when no answer of Jev's chose the
// state (a pinned move or a fallback), low below ConfidenceModestFrom, modest
// from there to ConfidenceHighFrom, high from there to the decision threshold,
// and binding at or above it.
const (
	ConfidenceNone    = "none"
	ConfidenceLow     = "low"
	ConfidenceModest  = "modest"
	ConfidenceHigh    = "high"
	ConfidenceBinding = "binding"

	ConfidenceModestFrom = 0.5
	ConfidenceHighFrom   = 0.7
)

// ConfidenceBand returns the band of probability p, given the decision
// threshold that starts binding. A value that is not a probability has none.
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

// SpanRoute describes one decided exit for its osmia.transition.route span. Chosen
// is the state the subject is in once the exit is decided, so for a held exit the
// state it stays in. Occurrence identifies the exit, so deciding it twice sends
// one span; Unit is empty for a workstream exit and Judgment for an exit no
// judgment record is behind.
type SpanRoute struct {
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
