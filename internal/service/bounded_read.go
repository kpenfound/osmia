package service

import (
	"context"
	"sync"
	"time"
)

// coreReadBound is how long a core read endpoint waits for a fresh result
// before it falls back to the last one it produced, so a request that
// cannot refresh within it answers from that snapshot, marked stale,
// instead of blocking past the bound.
const coreReadBound = time.Second

// StaleHeader marks a response assembled from an earlier snapshot because
// the fresh read did not finish within coreReadBound, or because the
// request itself ended first. Its value is always "true" when present.
const StaleHeader = "X-Osmia-Stale"

// viewCache holds the last value a bounded view produced, so a read that
// cannot refresh within its bound can still answer with it. The zero value
// has nothing cached.
type viewCache[T any] struct {
	mu    sync.Mutex
	value T
	has   bool
}

func (c *viewCache[T]) get() (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, c.has
}
func (c *viewCache[T]) set(v T) {
	c.mu.Lock()
	c.value, c.has = v, true
	c.mu.Unlock()
}

// boundedView runs fn in the background and returns its value, with stale
// false, if fn finishes before bound elapses or ctx ends. Otherwise it
// returns cache's last value, with stale true. Either way fn keeps running
// in the background and its eventual result replaces cache for the next
// read: boundedView never cancels fn itself, only its own wait for fn, so a
// read that is merely slow still refreshes the cache once it finishes. fn
// must not depend on ctx for anything beyond what it is safe to keep doing
// after this call returns; pass it a context detached from the request's
// own, such as context.Background, when fn needs one. If cache holds
// nothing yet, boundedView waits for fn regardless of bound, so the very
// first read never answers with a zero value falsely marked as a snapshot;
// it still returns as soon as ctx ends.
func boundedView[T any](ctx context.Context, cache *viewCache[T], bound time.Duration, fn func() T) (value T, stale bool) {
	done := make(chan T, 1)
	go func() {
		v := fn()
		cache.set(v)
		done <- v
	}()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case v := <-done:
		return v, false
	case <-ctx.Done():
	case <-timer.C:
	}
	if v, ok := cache.get(); ok {
		return v, true
	}
	select {
	case v := <-done:
		return v, false
	case <-ctx.Done():
		var zero T
		return zero, true
	}
}
