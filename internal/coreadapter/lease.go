package coreadapter

import (
	"context"
	"sync"
)

type releaseLease struct {
	mu       sync.Mutex
	released bool
	release  func(context.Context) error
}

func (l *releaseLease) Release(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	if err := l.release(ctx); err != nil {
		return err
	}
	l.released = true
	return nil
}
