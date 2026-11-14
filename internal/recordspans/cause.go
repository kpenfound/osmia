package recordspans

import (
	"strings"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

// The causes of a unit state transition: the bounded vocabulary of the cause
// attribute of a unit state span. A cause says why the unit left the state, so
// a unit returned to the mason is told apart by it: review-rejected, checks-failed,
// rebase-conflict and contest-ruled each return a unit to implementing.
const (
	CauseDispatched        = "dispatched"
	CauseDependenciesMet   = "dependencies-met"
	CauseCandidateReported = "candidate-reported"
	CauseRebased           = "rebased"
	CauseChecksPassed      = "checks-passed"
	CauseChecksFailed      = "checks-failed"
	CauseChecksIncomplete  = "checks-incomplete"
	CauseChecksRetry       = "checks-retry"
	CauseReviewApproved    = "review-approved"
	CauseReviewRejected    = "review-rejected"
	CauseReviewStale       = "review-stale"
	CauseReviewRetry       = "review-retry"
	CauseReviewContested   = "review-contested"
	CauseRebaseConflict    = "rebase-conflict"
	CauseContested         = "contested"
	CauseContestRuled      = "contest-ruled"
	CauseLanded            = "landed"
	CauseAmendmentWait     = "amendment-wait"
	CauseAmendmentResolved = "amendment-resolved"
	CauseMoved             = "moved"
	CauseOther             = "other"
)

// The actors whose transitions are told apart. They are the IDs of the service
// roles that record a unit's transitions.
const (
	actorChecks   = "checks"
	actorReviewer = "reviewer"
	actorMason    = "mason"
)

// UnitCauses lists every value UnitCause returns.
var UnitCauses = []string{CauseDispatched, CauseDependenciesMet, CauseCandidateReported, CauseRebased, CauseChecksPassed, CauseChecksFailed, CauseChecksIncomplete, CauseChecksRetry, CauseReviewApproved, CauseReviewRejected, CauseReviewStale, CauseReviewRetry, CauseReviewContested, CauseRebaseConflict, CauseContested, CauseContestRuled, CauseLanded, CauseAmendmentWait, CauseAmendmentResolved, CauseMoved, CauseOther}

// UnitCause returns the cause of unit state transition t from the states it
// moves between, the actor that recorded it and what its provenance names. It
// never returns text from the transition's reason.
func UnitCause(t trace.Transition) string {
	from, to := t.From, t.To
	chiefOrOwner := t.Actor.Kind == "owner" || t.Actor.ID == trace.ChiefOfStaff
	switch {
	case from == intake.UnitContested && to != intake.UnitContested:
		return CauseContestRuled
	case chiefOrOwner:
		return CauseMoved
	case to == intake.UnitImplementing && strings.HasSuffix(t.Cause, "-conflicted"):
		return CauseRebaseConflict
	}
	switch to {
	case intake.UnitReady:
		return CauseDependenciesMet
	case intake.UnitImplementing:
		switch from {
		case intake.UnitReady:
			return CauseDispatched
		case intake.UnitChecking:
			return CauseChecksFailed
		case intake.UnitReviewing:
			return CauseReviewRejected
		case intake.UnitWaiting:
			return CauseAmendmentResolved
		}
	case intake.UnitChecking:
		switch from {
		case intake.UnitImplementing:
			if t.Actor.ID == actorMason {
				return CauseCandidateReported
			}
			return CauseRebased
		case intake.UnitReviewing:
			return CauseReviewStale
		case intake.UnitApproved:
			return CauseRebased
		case intake.UnitChecking:
			return CauseChecksRetry
		}
	case intake.UnitReviewing:
		switch from {
		case intake.UnitChecking:
			return CauseChecksPassed
		case intake.UnitReviewing:
			return CauseReviewRetry
		}
	case intake.UnitApproved:
		return CauseReviewApproved
	case intake.UnitMerged:
		return CauseLanded
	case intake.UnitWaiting:
		return CauseAmendmentWait
	case intake.UnitContested:
		switch t.Actor.ID {
		case actorChecks:
			return CauseChecksIncomplete
		case actorReviewer:
			return CauseReviewContested
		}
		return CauseContested
	}
	return CauseOther
}

// The causes of a workstream state transition: who moved the workstream out of
// the state.
const (
	CauseOwner   = "owner"
	CauseChief   = "chief"
	CauseService = "service"
	CauseAgent   = "agent"
)

// FeatureCause returns who recorded workstream state transition t, which says
// whether an owner gate was ended by the owner.
func FeatureCause(t trace.Transition) string {
	switch {
	case t.Actor.Kind == "owner":
		return CauseOwner
	case t.Actor.ID == trace.ChiefOfStaff:
		return CauseChief
	case t.Actor.Kind == "agent":
		return CauseAgent
	}
	return CauseService
}
