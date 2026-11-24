package execution

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
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

func Exhausted(q trace.QueuedTurn) bool {
	return !q.CompletedAt.IsZero() && q.Status() == "failed" && q.Response.FailureClass == coreadapter.Infrastructure
}

func FailureHistory(q trace.QueuedTurn) string {
	type run struct {
		first, last         int
		profile, class, why string
	}
	var runs []run
	for _, a := range q.Attempts {
		failure := []rune(a.Failure)
		if len(failure) > 300 {
			failure = append(failure[:300], '…')
		}
		r := run{a.Number, a.Number, a.Profile.Name, string(a.FailureClass), string(failure)}
		if n := len(runs); n > 0 && runs[n-1].profile == r.profile && runs[n-1].class == r.class && runs[n-1].why == r.why {
			runs[n-1].last = a.Number
			continue
		}
		runs = append(runs, r)
	}
	attempts := make([]string, 0, len(runs))
	for _, r := range runs {
		numbers := fmt.Sprintf("attempt %d", r.first)
		if r.last != r.first {
			numbers = fmt.Sprintf("attempts %d to %d", r.first, r.last)
		}
		attempts = append(attempts, fmt.Sprintf("%s on profile %s: %s failure: %s", numbers, r.profile, r.class, r.why))
	}
	return strings.Join(attempts, "; ")
}

func FailureContestID(response string) string { return trace.EventID(response, "contested") }

func contestFailure(ctx context.Context, s *Controller, repo *trace.Repository, stream config.WorkstreamID, unit string, state trace.WorkflowState, from string, actor trace.Actor, role string, last trace.QueuedTurn, at time.Time) (bool, error) {
	if !Exhausted(last) {
		return false, nil
	}
	id := FailureContestID(last.Response.ID)
	transitions, err := trace.Read[trace.Transition](repo, stream)
	if err != nil || slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == id }) {
		return false, err
	}
	reason := fmt.Sprintf("the %s turn %s of unit %s failed on every retry and fallback profile: %s", role, last.Request.TurnID, unit, FailureHistory(last))
	return true, s.contestTurn(ctx, repo, stream, unit, state, from, actor, role, last, id, reason, "the infrastructure failure repeats", true)
}

func (s *Controller) contestTurn(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit string, state trace.WorkflowState, from string, actor trace.Actor, role string, last trace.QueuedTurn, id, reason, failure string, retry bool) error {
	targets := []string{UnitContested}
	if retry && (from == UnitImplementing || from == UnitReviewing) {
		targets = append(targets, from)
	}
	_, err := s.raise(ctx, true, UnitExitInput{
		Repo: repo, Stream: stream, Unit: unit, State: state, Trigger: TriggerTurnFailed, Cause: last.Response.ID,
		Default: UnitContested, Targets: targets, Actor: actor,
		ID: id, HeaderCause: last.Response.ID, Reason: reason, Events: []trace.Event{trace.Notice(id, "unit", reason)},
		Note:     failure,
		Evidence: router.Evidence{Facts: map[string]any{"role": role}, Records: []router.Entry{{Kind: "turn", ID: last.Request.TurnID, Text: reason}}},
	})
	return err
}

const MaxRecoveries = 3

func InterruptedInARow(th trace.Thread) int {
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

func contestInterrupted(ctx context.Context, s *Controller, repo *trace.Repository, stream config.WorkstreamID, unit string, state trace.WorkflowState, from string, actor trace.Actor, role string, th trace.Thread, at time.Time) (bool, error) {
	n := InterruptedInARow(th)
	if n < MaxRecoveries {
		return false, nil
	}
	last := th.Turns[len(th.Turns)-1]
	id := trace.EventID(last.Response.ID, "contested")
	reason := fmt.Sprintf("the latest %d %s turns of unit %s were interrupted in a row, the latest %s, so the unit is contested instead of its work continuing again", n, role, unit, last.Request.TurnID)
	return true, s.contestTurn(ctx, repo, stream, unit, state, from, actor, role, last, id, reason, fmt.Sprintf("%d turns in a row were interrupted", n), false)
}

func (m *masons) contestFailure(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (bool, error) {
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(th.Turns) == 0 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if contested, err := contestInterrupted(ctx, m.s, m.repository, stream, unit, state, UnitImplementing, masonActor, masonRole, th, m.s.Now()); err != nil || contested {
		return contested, err
	}
	return contestFailure(ctx, m.s, m.repository, stream, unit, state, UnitImplementing, masonActor, masonRole, th.Turns[len(th.Turns)-1], m.s.Now())
}

func FailedReview(contest trace.Transition, unit string) bool {
	return contest.To == UnitContested && contest.From == UnitReviewing && contest.Actor == ReviewerActor && contest.Cause != ReviewDocument(unit)
}

func ChecksContest(contest trace.Transition, unit string) bool {
	return contest.To == UnitContested && contest.From == UnitChecking && contest.Actor == ChecksActor && contest.Cause != ReviewDocument(unit)
}

func RebaseContest(contest trace.Transition, unit string) bool {
	return contest.To == UnitContested && contest.Actor == foremanActor && strings.HasPrefix(contest.Cause, RebaseSubject(unit)+"-") && strings.HasSuffix(contest.Cause, "-refused")
}

func BounceContest(contest trace.Transition, unit string) bool {
	if contest.To != UnitContested || contest.Cause != ReviewDocument(unit) {
		return false
	}
	return contest.From == UnitReviewing && contest.Actor == ReviewerActor || contest.From == UnitChecking && contest.Actor == ChecksActor
}
