package trace

import (
	"context"
	"errors"
	"fmt"
)

// ErrFeatureState reports a feature state a transition refuses to leave.
var ErrFeatureState = errors.New("feature state refuses the transition")

// NoticeKind is the kind of outbox events delivered to a workstream's chief of
// staff as information.
const NoticeKind = "notice"

// Notice returns an outbox event for the chief of staff, identified by the
// transition that raises it and a key unique within that transition.
func Notice(transition, key, body string) Event {
	return Event{ID: EventID(transition, key), Kind: NoticeKind, Body: body}
}

// SetFeatureState moves the workstream's FeatureSubject state to the given
// value and records a notice for the chief of staff in the same commit. The
// header identifies the transition; a retry with the same header, value and
// reason returns the state it committed.
func (r *Repository) SetFeatureState(ctx context.Context, h Header, to, reason string) (WorkflowState, error) {
	return r.SetFeatureStateUnless(ctx, h, to, reason)
}

// SetFeatureStateUnless is SetFeatureState, except that it refuses with
// ErrFeatureState, writing nothing, when the current state is one of refused.
// The check and the write hold the same lock.
func (r *Repository) SetFeatureStateUnless(ctx context.Context, h Header, to, reason string, refused ...string) (WorkflowState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return WorkflowState{}, err
	}
	_, v, err := r.loadWorkflow(h.Workstream)
	if err != nil {
		return WorkflowState{}, err
	}
	if old, ok := v.transactions[h.ID]; ok {
		if old.Transition.Subject != FeatureSubject || !equalJSON(old.Transition.Header, h) || old.Transition.To != to || old.Transition.Reason != reason {
			return WorkflowState{}, ErrConflict
		}
		return WorkflowState{Version: old.ExpectedVersion + 1, Value: to}, nil
	}
	state := v.states[FeatureSubject]
	for _, value := range refused {
		if state.Value == value {
			return state, ErrFeatureState
		}
	}
	body := fmt.Sprintf("Workstream state changed to %s: %s", to, reason)
	if state.Value != "" {
		body = fmt.Sprintf("Workstream state changed from %s to %s: %s", state.Value, to, reason)
	}
	return r.transact(ctx, Transaction{ExpectedVersion: state.Version,
		Transition: Transition{Header: h, Subject: FeatureSubject, From: state.Value, To: to, Reason: reason},
		Events:     []Event{Notice(h.ID, "state", body)}})
}
