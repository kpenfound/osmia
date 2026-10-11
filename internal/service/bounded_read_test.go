package service

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
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
	var cache viewCache[int]
	cache.set(1)

	release := make(chan struct{})
	slow := func() int {
		<-release
		return 2
	}

	begin := time.Now()
	v, stale := boundedView(context.Background(), &cache, 50*time.Millisecond, slow)
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
		if got, ok := cache.get(); ok && got == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slow read never refreshed the cache")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A read faster than the bound returns its own fresh value, not the cache.
	v, stale = boundedView(context.Background(), &cache, time.Second, func() int { return 3 })
	if stale || v != 3 {
		t.Fatalf("expected the fresh value with stale=false, got %d stale=%v", v, stale)
	}

	// A caller whose own context ends first gives up without waiting for
	// the bound or for fn, which here would otherwise block forever, and
	// still gets the cache's current value.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	never := make(chan struct{})
	v, stale = boundedView(ctx, &cache, time.Minute, func() int {
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
	var cache viewCache[int]
	release := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); close(release) }()
	v, stale := boundedView(context.Background(), &cache, time.Millisecond, func() int {
		<-release
		return 7
	})
	if stale || v != 7 {
		t.Fatalf("expected the first read awaited in full, got %d stale=%v", v, stale)
	}
}

// httpStatus opens a fresh connection to s's socket and returns GET
// /v1/status's status code and headers, so a test can check the
// stale-fallback header, which the Client type discards.
func httpStatus(t *testing.T, s *Service) (int, http.Header) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s.Socket())
	}}}
	resp, err := client.Get("http://osmia" + Prefix + "/status")
	must(t, err)
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// TestStatusAnswersWithinBoundAndMarksAFallbackSnapshotStale covers spec#1
// and spec#8: with a prior snapshot cached, a /status read that cannot
// refresh within coreReadBound (here because a factory read it shares a
// lock with is stalled through the existing boundary hook) still answers
// within the bound, from that snapshot, with the service's stale-fallback
// header set, rather than blocking until the slow read finishes.
func TestStatusAnswersWithinBoundAndMarksAFallbackSnapshotStale(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, demoStart, serviceActor)
	must(t, err)
	must(t, repository.CreateWorkstream(context.Background(), stream, demoStart, serviceActor))
	must(t, repository.Close())
	s, c := start(t, opts)
	_, active := s.runtimeOf(project)
	if active == nil {
		t.Fatal("no active project")
	}

	first, err := c.Statuses(context.Background())
	must(t, err)
	if len(first.Workstreams) != 1 || first.Workstreams[0].State != nil {
		t.Fatalf("unexpected first snapshot: %+v", first.Workstreams)
	}
	featureTransition(t, active.repository, stream, "pending", demoStart.Add(time.Minute))

	release := make(chan struct{})
	s.boundary = func(name string) error {
		if name == "status-agents" {
			<-release
		}
		return nil
	}
	defer func() { s.boundary = nil }()

	started := time.Now()
	code, headers := httpStatus(t, s)
	if elapsed := time.Since(started); elapsed > coreReadBound+500*time.Millisecond {
		t.Fatalf("a stalled status read blocked past its bound: %s", elapsed)
	}
	if code != 200 {
		t.Fatalf("unexpected status code %d", code)
	}
	if headers.Get(StaleHeader) != "true" {
		t.Fatal("expected the fallback response marked stale")
	}
	close(release)

	// The slow read was not cancelled: it refreshes the cache, so the next
	// read is fresh and no longer marked stale.
	deadline := time.Now().Add(10 * time.Second)
	for {
		fresh, err := c.Statuses(context.Background())
		must(t, err)
		if fresh.Workstreams[0].State != nil && *fresh.Workstreams[0].State == "pending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background refresh never landed in the cache")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, headers := httpStatus(t, s); headers.Get(StaleHeader) != "" {
		t.Fatalf("expected a fresh response to carry no stale header, got %q", headers.Get(StaleHeader))
	}
}
