package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/trace"
)

func httpStatus(t *testing.T, s *Service) (int, http.Header) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s.Socket())
	}}}
	resp, err := client.Get("http://osmia" + Prefix + "/status")
	intakefixture.Must(t, err)
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header
}

func TestStatusAnswersWithinBoundAndMarksAFallbackSnapshotStale(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	cfg, err := projectstore.LoadConfig(opts.Config)
	intakefixture.Must(t, err)
	repository, err := trace.Create(context.Background(), cfg.Root, cfg.Project, fixturekit.DemoStart, serviceActor)
	intakefixture.Must(t, err)
	intakefixture.Must(t, repository.CreateWorkstream(context.Background(), intakefixture.Stream, fixturekit.DemoStart, serviceActor))
	intakefixture.Must(t, repository.Close())
	s, c := start(t, opts)
	_, active := s.runtimeOf(intakefixture.Project)
	if active == nil {
		t.Fatal("no active project")
	}

	first, err := c.Statuses(context.Background())
	intakefixture.Must(t, err)
	if len(first.Workstreams) != 1 || first.Workstreams[0].State != nil {
		t.Fatalf("unexpected first snapshot: %+v", first.Workstreams)
	}
	featureTransition(t, active.repository, intakefixture.Stream, "pending", fixturekit.DemoStart.Add(time.Minute))

	release := make(chan struct{})
	s.setBoundary(func(name string) error {
		if name == "status-agents" {
			<-release
		}
		return nil
	})
	defer func() { s.setBoundary(nil) }()

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

	intakefixture.Soon(t, "the background refresh to land in the cache", func() bool {
		fresh, err := c.Statuses(context.Background())
		intakefixture.Must(t, err)
		return fresh.Workstreams[0].State != nil && *fresh.Workstreams[0].State == "pending"
	})
	if _, headers := httpStatus(t, s); headers.Get(StaleHeader) != "" {
		t.Fatalf("expected a fresh response to carry no stale header, got %q", headers.Get(StaleHeader))
	}
}
