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
