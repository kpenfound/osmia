package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

const questionWaitContestCause = "question-wait-reconciliation"

func latestUnitTransition(repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.Transition, error) {
	transitions, err := trace.Read[trace.Transition](repository, stream)
	if err != nil {
		return trace.Transition{}, err
	}
	for _, t := range slices.Backward(transitions) {
		if t.Subject == trace.UnitSubject(unit) {
			return t, nil
		}
	}
	return trace.Transition{}, nil
}

// questionWaitOwner follows the durable question cause as well as the actor:
// an owner or recovery move can preserve a worker's question wait.
func questionWaitOwner(repository *trace.Repository, stream config.WorkstreamID, unit, agent string, actor trace.Actor) (bool, error) {
	latest, err := latestUnitTransition(repository, stream, unit)
	if err != nil || strings.HasPrefix(latest.Cause, "amendment_") {
		return false, err
	}
	asked, err := repository.Questions(stream)
	if err != nil {
		return false, err
	}
	for _, q := range asked {
		if questionWaitCause(latest.Cause, q.Asked.ID) {
			return q.Asked.Thread == agent, nil
		}
	}
	if latest.Actor == actor {
		return true, nil
	}
	// An unidentified wait with a delivered worker answer needs a visible
	// recovery decision even when a different actor recorded the wait.
	if !slices.ContainsFunc(asked, func(q trace.QuestionState) bool { return q.Asked.Thread == agent && q.State == trace.QuestionAnswered }) {
		return false, nil
	}
	th, err := repository.Thread(stream, agent)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(asked, func(q trace.QuestionState) bool { return q.Asked.Thread == agent && questionDelivered(q, th) }), nil
}

func questionWaitCause(cause, question string) bool {
	return cause == trace.QuestionSubject(question)+"_"+trace.QuestionOpen || cause == trace.QuestionSubject(question)+"_"+trace.QuestionAnswered
}

func questionDelivered(q trace.QuestionState, th trace.Thread) bool {
	return q.State == trace.QuestionAnswered && q.Ruling != nil && slices.ContainsFunc(th.Turns, func(t trace.QueuedTurn) bool {
		return t.Request.TurnID == questions.TurnID(q.Asked.ID) && t.Request.Cause == trace.QuestionSubject(q.Asked.ID)+"_"+trace.QuestionAnswered
	})
}

// waitedQuestion reads the question named by the wait, independent of the
// thread's latest turn. Later turns cannot hide a delivered answer.
func waitedQuestion(repository *trace.Repository, stream config.WorkstreamID, unit string, th trace.Thread, asked []trace.QuestionState) (trace.QuestionState, bool, string, error) {
	latest, err := latestUnitTransition(repository, stream, unit)
	if err != nil {
		return trace.QuestionState{}, false, "", err
	}
	for _, q := range asked {
		if q.Asked.Thread == th.Identity.ThreadID && questionWaitCause(latest.Cause, q.Asked.ID) {
			if !slices.ContainsFunc(th.Turns, func(t trace.QueuedTurn) bool { return t.Request.TurnID == q.Asked.Turn }) {
				if questionDelivered(q, th) {
					return q, false, "the delivered question has no asking turn", nil
				}
				return q, false, "", nil
			}
			return q, questionDelivered(q, th), "", nil
		}
	}
	for _, q := range asked {
		if q.Asked.Thread == th.Identity.ThreadID && questionDelivered(q, th) {
			return q, false, "the waiting transition does not identify the worker's question", nil
		}
	}
	return trace.QuestionState{}, false, "", nil
}

func (m *masons) contestQuestionWait(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, actor trace.Actor, reason string) error {
	id := fmt.Sprintf("%s-contested-question-%d", trace.UnitSubject(unit), state.Version)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.Now(), Actor: actor, Cause: questionWaitContestCause}
	reason = "cannot reconcile answered question wait: " + reason
	_, err := m.repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(unit), From: state.Value, To: UnitContested, Reason: reason}, Events: []trace.Event{trace.Notice(id, "unit", "Unit "+unit+" is contested: "+reason)}})
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}
