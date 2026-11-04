package trace

import (
	"context"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
)

type turnAdmissionKey struct{}

type turnAdmission struct {
	stream         config.WorkstreamID
	subject, state string
}

// WithTurnAdmission requires new turn enqueues and claims in stream to observe
// subject in state. The check shares the repository lock with workflow writes,
// so a transition that ends the work prevents subsequent turn admission.
// Replaying an accepted request or claim and finishing a claimed turn remain
// possible. The condition follows the operation's context through dispatch,
// including continuations; callers restore it when replaying the operation.
func WithTurnAdmission(ctx context.Context, stream config.WorkstreamID, subject, state string) context.Context {
	return context.WithValue(ctx, turnAdmissionKey{}, turnAdmission{stream, subject, state})
}

// admitTurn requires r.mu and the workflow view loaded under that lock.
func admitTurn(ctx context.Context, stream config.WorkstreamID, view *workflowView) error {
	condition, ok := ctx.Value(turnAdmissionKey{}).(turnAdmission)
	if !ok || condition.stream != stream {
		return nil
	}
	if got := view.states[condition.subject].Value; got != condition.state {
		return fmt.Errorf("%w: turn admission requires %s to be %s, now %s", ErrConflict, condition.subject, condition.state, got)
	}
	return nil
}
