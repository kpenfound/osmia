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
	return r.setFeatureState(ctx, h, nil, to, reason, nil)
}

// MoveFeatureState is SetFeatureState from an expected current state: it
// refuses with ErrConflict when the workstream is in another state, unless
// the same transition was already recorded.
func (r *Repository) MoveFeatureState(ctx context.Context, h Header, from, to, reason string) (WorkflowState, error) {
	return r.setFeatureState(ctx, h, &from, to, reason, nil)
}

// SetFeatureStateUnless is SetFeatureState, except that it refuses with
// ErrFeatureState, writing nothing, when the current state is one of refused,
// even for a retry of a transition that already committed.
// The check and the write hold the same lock.
func (r *Repository) SetFeatureStateUnless(ctx context.Context, h Header, to, reason string, refused ...string) (WorkflowState, error) {
	return r.setFeatureState(ctx, h, nil, to, reason, refused)
}

func (r *Repository) setFeatureState(ctx context.Context, h Header, from *string, to, reason string, refused []string) (WorkflowState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return WorkflowState{}, err
	}
	_, v, err := r.loadWorkflow(h.Workstream)
	if err != nil {
		return WorkflowState{}, err
	}
	state := v.states[FeatureSubject]
	for _, value := range refused {
		if state.Value == value {
			return state, ErrFeatureState
		}
	}
	if old, ok := v.transactions[h.ID]; ok {
		if old.Transition.Subject != FeatureSubject || !equalJSON(old.Transition.Header, h) || old.Transition.To != to || old.Transition.Reason != reason {
			return WorkflowState{}, ErrConflict
		}
		return WorkflowState{Version: old.ExpectedVersion + 1, Value: to}, nil
	}
	if from != nil && state.Value != *from {
		return WorkflowState{}, fmt.Errorf("%w: workstream is %q, not %q", ErrConflict, state.Value, *from)
	}
	body := fmt.Sprintf("Workstream state changed to %s: %s", to, reason)
	if state.Value != "" {
		body = fmt.Sprintf("Workstream state changed from %s to %s: %s", state.Value, to, reason)
	}
	return r.transact(ctx, Transaction{ExpectedVersion: state.Version,
		Transition: Transition{Header: h, Subject: FeatureSubject, From: state.Value, To: to, Reason: reason},
		Events:     []Event{Notice(h.ID, "state", body)}})
}
