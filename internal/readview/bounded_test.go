package readview

import (
	"context"
	"testing"
	"time"
)

// TestBoundedViewFallsBackToTheCacheWithinItsBoundAndRefreshesItInTheBackground
// covers spec#1 and spec#8's pending-view requirement directly: a read that
// cannot finish within its bound returns the cache's last value, marked
// stale, instead of blocking past the bound; the slow read is never
// cancelled by that bound, so it still lands in the cache for the next
// caller; a read faster than the bound returns its own fresh value; and a
// caller whose own context ends gives up waiting without affecting the
// background read.
func TestBoundedViewFallsBackToTheCacheWithinItsBoundAndRefreshesItInTheBackground(t *testing.T) {
	t.Parallel()
	var cache Cache[int]
	cache.Set(1)

	release := make(chan struct{})
	slow := func() int {
		<-release
		return 2
	}

	begin := time.Now()
	v, stale := Bounded(context.Background(), &cache, 50*time.Millisecond, slow)
	if elapsed := time.Since(begin); elapsed > 500*time.Millisecond {
		t.Fatalf("boundedView blocked past its bound: %s", elapsed)
	}
	if !stale || v != 1 {
		t.Fatalf("expected the cached value marked stale, got %d stale=%v", v, stale)
	}

	// The slow read was not cancelled: it still refreshes the cache.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got, ok := cache.Get(); ok && got == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slow read never refreshed the cache")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A read faster than the bound returns its own fresh value, not the cache.
	v, stale = Bounded(context.Background(), &cache, time.Second, func() int { return 3 })
	if stale || v != 3 {
		t.Fatalf("expected the fresh value with stale=false, got %d stale=%v", v, stale)
	}

	// A caller whose own context ends first gives up without waiting for
	// the bound or for fn, which here would otherwise block forever, and
	// still gets the cache's current value.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	never := make(chan struct{})
	v, stale = Bounded(ctx, &cache, time.Minute, func() int {
		<-never
		return 4
	})
	if !stale || v != 3 {
		t.Fatalf("expected a cancelled caller to get the cache's current value, got %d stale=%v", v, stale)
	}
}

// TestBoundedViewWaitsForTheFirstReadRegardlessOfBound covers the one case
// where boundedView must not answer with a snapshot: before anything has
// ever been cached, a slow read is still awaited, so the first ever read
// never answers with a zero value falsely claimed as a snapshot.
func TestBoundedViewWaitsForTheFirstReadRegardlessOfBound(t *testing.T) {
	t.Parallel()
	var cache Cache[int]
	release := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); close(release) }()
	v, stale := Bounded(context.Background(), &cache, time.Millisecond, func() int {
		<-release
		return 7
	})
	if stale || v != 7 {
		t.Fatalf("expected the first read awaited in full, got %d stale=%v", v, stale)
	}
}
