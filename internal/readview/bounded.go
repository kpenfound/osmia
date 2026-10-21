package readview

import (
	"context"
	"sync"
	"time"
)

// Cache holds the last value a bounded view produced, so a read that
// cannot refresh within its bound can still answer with it. The zero value
// has nothing cached.
type Cache[T any] struct {
	mu         sync.Mutex
	value      T
	has        bool
	generation uint64
}

func (c *Cache[T]) Get() (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, c.has
}
func (c *Cache[T]) Set(v T) {
	c.mu.Lock()
	c.value, c.has = v, true
	c.mu.Unlock()
}

// Invalidate updates the cached value under its lock and prevents background
// reads already in flight from replacing it. It preserves whether a value
// has been cached.
func (c *Cache[T]) Invalidate(update func(T) T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	c.value = update(c.value)
}

// Bounded runs fn in the background and returns its value, with stale
// false, if fn finishes before bound elapses or ctx ends. Otherwise it
// returns cache's last value, with stale true. Either way fn keeps running
// in the background and its eventual result replaces cache for the next
// read unless the cache was invalidated. Bounded never cancels fn itself,
// only its own wait for fn, so a read that is merely slow still refreshes
// the cache once it finishes. fn
// must not depend on ctx for anything beyond what it is safe to keep doing
// after this call returns; pass it a context detached from the request's
// own, such as context.Background, when fn needs one. If cache holds
// nothing yet, Bounded waits for fn regardless of bound, so the very
// first read never answers with a zero value falsely marked as a snapshot;
// it still returns as soon as ctx ends.
func Bounded[T any](ctx context.Context, cache *Cache[T], bound time.Duration, fn func() T) (value T, stale bool) {
	cache.mu.Lock()
	generation := cache.generation
	cache.mu.Unlock()
	done := make(chan T, 1)
	go func() {
		v := fn()
		cache.mu.Lock()
		if generation == cache.generation {
			cache.value, cache.has = v, true
		}
		cache.mu.Unlock()
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
	if v, ok := cache.Get(); ok {
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
