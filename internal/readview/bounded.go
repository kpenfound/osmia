// Package readview bounds and stabilises the reads behind the service's
// views: Bounded answers within a time limit from a cache, and Consistent
// retries a read until the trace and runtime generations it reads stay put.
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
	running    *refresh[T]
	queued     *refresh[T]
	queuedFn   func() T
}

type refresh[T any] struct {
	done  chan struct{}
	value T
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

func (c *Cache[T]) start(f *refresh[T], fn func() T) {
	c.running = f
	generation := c.generation
	go func() {
		v := fn()
		c.mu.Lock()
		defer c.mu.Unlock()
		if generation == c.generation {
			c.value, c.has = v, true
		}
		f.value = v
		close(f.done)
		c.running = nil
		if next := c.queued; next != nil {
			nextFn := c.queuedFn
			c.queued, c.queuedFn = nil, nil
			c.start(next, nextFn)
		}
	}()
}

func (c *Cache[T]) join(fn func() T) *refresh[T] {
	if c.running == nil {
		f := &refresh[T]{done: make(chan struct{})}
		c.start(f, fn)
		return f
	}
	if c.queued == nil {
		c.queued = &refresh[T]{done: make(chan struct{})}
	}
	c.queuedFn = fn
	return c.queued
}

// Bounded refreshes cache with fn in the background and returns the
// refreshed value, with stale false, if the refresh finishes before bound
// elapses or ctx ends. Otherwise it returns cache's last value, with stale
// true. The refresh it waits for begins no earlier than the call, so its
// value reflects every write that completed before the call. At most one
// refresh of a cache runs at a time: a call made while one is in flight
// waits for the single refresh queued behind it, shared with every other
// call made meanwhile. Either way the refresh keeps running in the
// background and its eventual result replaces cache for the next read
// unless the cache was invalidated. Bounded never cancels fn itself, only
// its own wait for fn, so a read that is merely slow still refreshes the
// cache once it finishes. fn must not depend on ctx for anything beyond what
// it is safe to keep doing after this call returns; pass it a context
// detached from the request's own, such as context.Background, when fn
// needs one. If cache holds nothing yet, Bounded waits for the refresh
// regardless of bound, so the very first read never answers with a zero
// value falsely marked as a snapshot; it still returns as soon as ctx ends.
func Bounded[T any](ctx context.Context, cache *Cache[T], bound time.Duration, fn func() T) (value T, stale bool) {
	cache.mu.Lock()
	f := cache.join(fn)
	cache.mu.Unlock()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.value, false
	case <-ctx.Done():
	case <-timer.C:
	}
	if v, ok := cache.Get(); ok {
		return v, true
	}
	select {
	case <-f.done:
		return f.value, false
	case <-ctx.Done():
		var zero T
		return zero, true
	}
}
