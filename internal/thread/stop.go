package thread

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/trace"
)

// Stop is the cause with which the service stops a turn on purpose.
type Stop struct{ trace.TurnStop }

func (s *Stop) Error() string {
	return fmt.Sprintf("turn stopped by a %s pause: %s", s.Scope, s.Reason)
}

type executionKey struct{}

// Stoppable returns ctx carrying a separate context for the turn's agent
// session, and the function that stops that session with a cause. Stopping
// cancels the session only: the claim, the captured result and the completion
// are still recorded under ctx, so RunNext completes a stopped turn as
// interrupted with its Stop and no failure. A turn stopped before its session
// starts completes the same way without running. Stopping twice keeps the
// first cause.
func Stoppable(ctx context.Context) (context.Context, func(*Stop)) {
	execution, cancel := context.WithCancelCause(ctx)
	return context.WithValue(ctx, executionKey{}, execution), func(s *Stop) { cancel(s) }
}

// execution returns the context the turn's agent session runs under.
func execution(ctx context.Context) context.Context {
	if e, ok := ctx.Value(executionKey{}).(context.Context); ok {
		return e
	}
	return ctx
}

// stopped returns the Stop the session context was cancelled with, or nil.
func stopped(ctx context.Context) *Stop {
	var s *Stop
	if errors.As(context.Cause(ctx), &s) {
		return s
	}
	return nil
}
