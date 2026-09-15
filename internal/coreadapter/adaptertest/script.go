// Package adaptertest supplies deterministic, process-free port fixtures.
package adaptertest

import (
	"context"
	"errors"
	"sync"
)

var ErrExhausted = errors.New("adapter fixture exhausted")

type Reply[T any] struct {
	Value T
	Err   error
}

// Script consumes one reply per non-cancelled call, retaining arguments in call
// order. Cancellation records nothing and consumes nothing. Results and requests
// are shallow copies: callers must not mutate nested values after sharing them.
// The zero value fails closed with ErrExhausted, never an implicit success.
type Script[Q, R any] struct {
	mu      sync.Mutex
	replies []Reply[R]
	calls   []Q
}

func NewScript[Q, R any](replies ...Reply[R]) *Script[Q, R] {
	return &Script[Q, R]{replies: append([]Reply[R](nil), replies...)}
}
func (s *Script[Q, R]) Call(ctx context.Context, request Q) (R, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero R
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	s.calls = append(s.calls, request)
	if len(s.replies) == 0 {
		return zero, ErrExhausted
	}
	reply := s.replies[0]
	s.replies = s.replies[1:]
	return reply.Value, reply.Err
}
func (s *Script[Q, R]) Calls() []Q {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Q(nil), s.calls...)
}
