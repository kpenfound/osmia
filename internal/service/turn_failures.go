package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// exhausted reports whether a completed turn failed with an infrastructure
// failure. The runner retries such a failure on the turn's profile and its
// fallbacks before it completes the turn, so a turn that still ended with one
// exhausted its retries.
func exhausted(q trace.QueuedTurn) bool {
	return !q.CompletedAt.IsZero() && q.Status() == "failed" && q.Response.FailureClass == coreadapter.Infrastructure
}

// failureHistory describes each attempt of a turn: its profile, and the class
// and text of its failure.
func failureHistory(q trace.QueuedTurn) string {
	var attempts []string
	for _, a := range q.Attempts {
		failure := []rune(a.Failure)
		if len(failure) > 300 {
			failure = append(failure[:300], '…')
		}
		attempts = append(attempts, fmt.Sprintf("attempt %d on profile %s: %s failure: %s", a.Number, a.Profile.Name, a.FailureClass, string(failure)))
	}
	return strings.Join(attempts, "; ")
}

// failureContestID is the ID of the transition that contests a unit because
// the turn that captured response failed.
func failureContestID(response string) string { return trace.EventID(response, "contested") }

// contestFailure contests a unit whose thread's latest turn exhausted its
// infrastructure retries and fallbacks, moving it from `from` with a notice
// for the chief of staff. The reason lists every attempt; the attempts stay in
// the thread's log. A turn that contested the unit once never does again, so
// an owner ruling moves the unit on. It reports whether the unit moved.
func contestFailure(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit string, state trace.WorkflowState, from string, actor trace.Actor, role string, last trace.QueuedTurn, at time.Time) (bool, error) {
	if !exhausted(last) {
		return false, nil
	}
	id := failureContestID(last.Response.ID)
	transitions, err := trace.Read[trace.Transition](repo, stream)
	if err != nil || slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == id }) {
		return false, err
	}
	reason := fmt.Sprintf("the %s turn %s of unit %s failed on every retry and fallback profile, so the unit is contested: %s", role, last.Request.TurnID, unit, failureHistory(last))
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: at, Actor: actor, Cause: last.Response.ID}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: from, To: UnitContested, Reason: reason}, Events: []trace.Event{trace.Notice(id, "unit", reason)}}
	if _, err := repo.Transact(ctx, tx); errors.Is(err, trace.ErrConflict) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// maxRecoveries is how many turns of one piece of work may be interrupted in
// a row, the turn and the continuations that recover it, before the service
// stops continuing it.
const maxRecoveries = 3

// interruptedInARow counts the interrupted turns of the thread's latest
// piece of work: its latest turn when that ended interrupted, and each turn
// before it that it continues, back to the turn that started the work. A turn
// that is no continuation, such as one a ruling or a move queued, starts a
// new count.
func interruptedInARow(th trace.Thread) int {
	n := 0
	for _, q := range slices.Backward(th.Turns) {
		if q.CompletedAt.IsZero() || q.Status() != "interrupted" {
			break
		}
		n++
		if !strings.Contains(q.Request.TurnID, "-recover-") {
			break
		}
	}
	return n
}

// contestInterrupted contests a unit whose thread's latest maxRecoveries
// turns were interrupted, moving it from `from` with a notice for the chief
// of staff, instead of continuing the work once more. It reports whether the
// unit moved.
func contestInterrupted(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit string, state trace.WorkflowState, from string, actor trace.Actor, role string, th trace.Thread, at time.Time) (bool, error) {
	n := interruptedInARow(th)
	if n < maxRecoveries {
		return false, nil
	}
	last := th.Turns[len(th.Turns)-1]
	id := trace.EventID(last.Response.ID, "contested")
	reason := fmt.Sprintf("the latest %d %s turns of unit %s were interrupted in a row, the latest %s, so the unit is contested instead of its work continuing again", n, role, unit, last.Request.TurnID)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: at, Actor: actor, Cause: last.Response.ID}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: from, To: UnitContested, Reason: reason}, Events: []trace.Event{trace.Notice(id, "unit", reason)}}
	if _, err := repo.Transact(ctx, tx); errors.Is(err, trace.ErrConflict) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// contestFailure contests an implementing unit whose mason's latest turn
// exhausted its infrastructure retries and fallbacks, or whose latest
// maxRecoveries turns were interrupted.
func (m *masons) contestFailure(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (bool, error) {
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(th.Turns) == 0 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if contested, err := contestInterrupted(ctx, m.repository, stream, unit, state, UnitImplementing, masonActor, masonRole, th, m.s.now()); err != nil || contested {
		return contested, err
	}
	return contestFailure(ctx, m.repository, stream, unit, state, UnitImplementing, masonActor, masonRole, th.Turns[len(th.Turns)-1], m.s.now())
}

// failedReview reports whether a contest was raised because a reviewer turn
// failed, rather than by review bounces.
func failedReview(contest trace.Transition, unit string) bool {
	return contest.To == UnitContested && contest.From == UnitReviewing && contest.Actor == reviewerActor && contest.Cause != reviewDocument(unit)
}

// checksContest reports whether a contest was raised because the unit's
// check runs kept failing to complete.
func checksContest(contest trace.Transition, unit string) bool {
	return contest.To == UnitContested && contest.From == UnitChecking && contest.Actor == checksActor && contest.Cause != reviewDocument(unit)
}

// bounceContest reports whether a contest was raised because send-backs, by
// reviewer verdicts and failed checks together, reached shed.max_bounces.
func bounceContest(contest trace.Transition, unit string) bool {
	if contest.To != UnitContested || contest.Cause != reviewDocument(unit) {
		return false
	}
	return contest.From == UnitReviewing && contest.Actor == reviewerActor || contest.From == UnitChecking && contest.Actor == checksActor
}
