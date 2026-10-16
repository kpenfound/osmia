package thread

import (
	"context"

	"github.com/kpenfound/osmia/internal/trace"
)

type claimObserverKey struct{}

// WithClaimObserver reports claims acquired by runners using the returned
// context. The observer runs synchronously after successful acquisition,
// outside trace locks, before execution starts. A failed acquisition reports
// nothing. Callers may use the token after their dispatcher returns to settle
// only the claim that invocation acquired.
func WithClaimObserver(ctx context.Context, observe func(trace.TurnRequest, trace.TurnClaim)) context.Context {
	return context.WithValue(ctx, claimObserverKey{}, observe)
}

func observeClaim(ctx context.Context, q trace.QueuedTurn) {
	if observe, ok := ctx.Value(claimObserverKey{}).(func(trace.TurnRequest, trace.TurnClaim)); ok && observe != nil {
		observe(q.Request, *q.Claim)
	}
}
