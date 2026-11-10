package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// continueTurnID is the ID of the turn that continues the mason turn with the
// given sequence after it ended without a report the service takes.
func continueTurnID(unit string, sequence uint64) string {
	return fmt.Sprintf("%s-continue-%d", masonAgent(unit), sequence)
}

// endedBy says what ended a failed turn: its limit, error or failure.
func endedBy(q trace.QueuedTurn) string {
	r := q.Response
	var parts []string
	if r.Result.TimedOut {
		parts = append(parts, "it ran out of time")
	}
	if r.Result.IsError && strings.Contains(r.Result.ErrorSubtype, "max_turns") {
		parts = append(parts, "it reached its turn limit")
	}
	if r.Failure != "" {
		failure := []rune(r.Failure)
		if len(failure) > 300 {
			failure = append(failure[:300], '…')
		}
		parts = append(parts, "it failed: "+string(failure))
	}
	if len(parts) == 0 {
		switch {
		case r.Result.IsError && r.Result.ErrorSubtype != "":
			parts = append(parts, "it ended with the error "+r.Result.ErrorSubtype)
		case r.Result.IsError:
			parts = append(parts, "it ended with an error")
		case r.Result.ExitCode != 0:
			parts = append(parts, fmt.Sprintf("its session exited with code %d", r.Result.ExitCode))
		case r.Result.Signal != 0:
			parts = append(parts, fmt.Sprintf("its session was ended by signal %d", r.Result.Signal))
		}
	}
	return strings.Join(parts, "; ")
}

// endingWithoutReport reports why a completed mason turn ended without an
// accepted done the service takes, when the unit's mason is to continue it:
// the service refused its latest done, or the turn failed without an
// infrastructure failure that used up its retries. A clean ending without
// done is classified, a question parks the unit, an interruption is recovered
// and an exhausted infrastructure failure is contested.
func endingWithoutReport(q trace.QueuedTurn) (string, bool) {
	if q.CompletedAt.IsZero() || q.Response == nil {
		return "", false
	}
	r := q.Response
	switch q.Status() {
	case "idle":
		if r.Result.Outcome != nil || r.Result.Refusal == "" {
			return "", false
		}
		return "the service refused its report: " + r.Result.Refusal, true
	case "failed":
		if exhausted(q) || r.Result.Outcome != nil {
			return "", false
		}
		reason := "the turn ended without a report because " + endedBy(q)
		if r.Result.Refusal != "" {
			reason += "; the service had refused its report: " + r.Result.Refusal
		}
		return reason, true
	}
	return "", false
}

// continuationPrompt is the prompt of the turn that continues the mason turn
// that ended as reason says. A turn whose report the service refused asks for
// the report again; any other ending asks the mason to check the unit's
// acceptance in its workspace and report.
func continuationPrompt(last trace.QueuedTurn, uncollected error) string {
	r := last.Response
	switch {
	case uncollected != nil:
		return fmt.Sprintf("You reported this unit done and the service accepted the report, but your last turn then ended because %s, and the service could not take your workspace as the unit's candidate: %v. Your workspace includes the files left by that turn. Check that it is whole, check the unit's acceptance, and call done again.", endedBy(last), uncollected)
	case r.Result.Refusal != "" && last.Status() == "idle":
		return fmt.Sprintf("The service refused the report you made with done: %s. Correct that, check the unit's acceptance in your workspace, and call done again.", r.Result.Refusal)
	}
	prompt := "Your last turn ended without a report the service could take because " + endedBy(last) + "."
	if r.Result.Refusal != "" {
		prompt += " The service had refused the report you made with done: " + r.Result.Refusal + "."
	}
	return prompt + " Your workspace includes the files left by that turn. Continue the unit from there, check its acceptance, and call done with the required report when it holds; call ask if you need a decision."
}

// countsAsAttempt reports whether a completed mason turn counts toward
// mason.max_clean_turns: it was classified, ended without a report the
// service takes, or has a continuation after it.
func countsAsAttempt(th trace.Thread, unit string, q trace.QueuedTurn) bool {
	if q.CompletedAt.IsZero() || q.Response == nil {
		return false
	}
	if q.Response.Classification != nil {
		return true
	}
	if _, ended := endingWithoutReport(q); ended {
		return true
	}
	id := continueTurnID(unit, q.Sequence)
	return slices.ContainsFunc(th.Turns, func(t trace.QueuedTurn) bool { return t.Request.TurnID == id })
}

// continueEnding follows a mason turn that ended without an accepted done
// the service takes, unless another handler owns the ending. It queues one
// continuation in the unit's existing workspace, or contests the unit once the
// turns since the latest ruling that ended without a report reach
// mason.max_clean_turns, with a notice for the chief of staff. uncollected is
// why the candidate of a unit whose accepted done the turn ended with a
// failure after could not be made. Both decisions have IDs derived from the
// turn's response, so a pass after a restart finishes them without a second
// continuation or contest. It reports whether a continuation is queued and
// whether the unit was contested.
func (m *masons) continueEnding(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, uncollected error) (queued, contested bool, err error) {
	th, err := m.repository.Thread(stream, masonAgent(unit))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(th.Turns) == 0 {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	last := th.Turns[len(th.Turns)-1]
	reason, ended := endingWithoutReport(last)
	if uncollected != nil && reportTaken(last) {
		reason, ended = fmt.Sprintf("the turn ended because %s after it reported done, and its candidate cannot be made: %v", endedBy(last), uncollected), true
	}
	if !ended {
		return false, false, nil
	}
	var reset uint64
	if ruling, found, err := latestMasonRuling(m.repository, stream, unit); err != nil {
		return false, false, err
	} else if found {
		reset = ruling.ResetTurn
	}
	if last.Sequence <= reset {
		return false, false, nil
	}
	turnID := continueTurnID(unit, last.Sequence)
	if slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turnID }) {
		return true, false, nil
	}
	attempts := 1
	for _, q := range th.Turns[:len(th.Turns)-1] {
		if q.Sequence > reset && countsAsAttempt(th, unit, q) {
			attempts++
		}
	}
	if attempts >= m.cfg.Mason.MaxCleanTurns {
		id := failureContestID(last.Response.ID)
		transitions, err := trace.Read[trace.Transition](m.repository, stream)
		if err != nil || slices.ContainsFunc(transitions, func(t trace.Transition) bool { return t.ID == id }) {
			return false, false, err
		}
		reason := fmt.Sprintf("mason turn %s of unit %s: %s; the mason had %d/%d turns that ended without a report the service took (mason.max_clean_turns), so the unit is contested instead of its work continuing again", last.Request.TurnID, unit, reason, attempts, m.cfg.Mason.MaxCleanTurns)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.Now(), Actor: masonActor, Cause: last.Response.ID}
		tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: UnitImplementing, To: UnitContested, Reason: reason}, Events: []trace.Event{trace.Notice(id, "unit", fmt.Sprintf("Unit %s is contested: %s.", unit, reason))}}
		if _, err := m.repository.Transact(ctx, tx); errors.Is(err, trace.ErrConflict) {
			return false, false, nil
		} else if err != nil {
			return false, false, err
		}
		return false, true, nil
	}
	profile, _, err := m.s.RoleExecution(m.cfg, masonRole)
	if err != nil {
		return false, false, err
	}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turnID, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.Now(), Actor: masonActor, Cause: last.Response.ID, Depth: last.Request.Depth + 1}, AgentID: masonAgent(unit), ThreadID: masonAgent(unit), TurnID: turnID, Profile: profile, SystemPrompt: last.Request.SystemPrompt, Prompt: continuationPrompt(last, uncollected)}
	err = m.enqueueTurn(ctx, req)
	return err == nil, false, err
}
