// Package servicetest starts and drives real services with fixture execution boundaries.
package servicetest

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// passingCloudJSON is the JSON of a completed "Run Dagger Checks" status in
// which every check passed.
const passingCloudJSON = `{"dagger":{"status":"completed","checks":{"total":3,"passed":3,"failed":0,"cancelled":0}}}`

type passingTransport struct{}

func (passingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(passingCloudJSON)), Request: r}, nil
}

// PassingCloudFetcher answers every read of a "Run Dagger Checks" JSON with a
// completed run in which every check passed, without a network.
func PassingCloudFetcher() service.CloudStatusFetcher {
	return service.CloudStatusFetcher{HTTP: &http.Client{Transport: passingTransport{}}, Allow: func(*url.URL) bool { return true }}
}

func Options(cfg config.Options) service.Options {
	return service.Options{Config: cfg, Build: api.Identity{Version: "test", Commit: "abc"}, Workstreams: []config.WorkstreamID{fixturekit.Stream}, ShutdownTimeout: 100 * time.Millisecond,
		CloudStatusFetcher: PassingCloudFetcher(), FinalChecksWait: fixturekit.NoPostPushWait()}
}
func FixtureAt(t *testing.T, home string) service.Options {
	t.Helper()
	return Options(fixturekit.ConfigAt(t, home))
}
func Fixture(t *testing.T) service.Options {
	t.Helper()
	home, err := os.MkdirTemp("", "os-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	return FixtureAt(t, home)
}
func ProjectFixture(t *testing.T) (service.Options, string) {
	t.Helper()
	cfg, clone := fixturekit.ProjectConfig(t)
	o := Options(cfg)
	o.Workstreams = nil
	return o, clone
}
func Start(t *testing.T, opts service.Options, hooks ...service.TestHooks) (*service.Service, *service.Client) {
	t.Helper()
	h := service.TestHooks{ReviewChecks: fixturekit.PassingChecks}
	if len(hooks) > 0 {
		h = hooks[0]
		if h.ReviewChecks == nil {
			h.ReviewChecks = fixturekit.PassingChecks
		}
	}
	s, err := service.StartForTest(context.Background(), opts, h)
	fixturekit.Must(t, err)
	c := service.NewClient(s.Socket())
	t.Cleanup(func() {
		c.Close()
		if err := s.Close(); err != nil {
			t.Logf("service close: %v", err)
		}
	})
	return s, c
}
func ConversationFixture(t *testing.T, prefix string) (service.Options, *config.Config) {
	t.Helper()
	cfg, loaded := fixturekit.Conversation(t, prefix)
	o := Options(cfg)
	o.Reconciliation.Now = (&fixturekit.DemoClock{At: fixturekit.DemoStart}).Now
	return o, loaded
}
func ActivityFixture(t *testing.T) (service.Options, *config.Config, *trace.Repository) {
	t.Helper()
	cfg, loaded, repo := fixturekit.Activity(t)
	return Options(cfg), loaded, repo
}
func WithWeb(t *testing.T, opts service.Options, addr string) {
	t.Helper()
	intakefixture.WithWeb(t, opts.Config.Root, addr)
}
func ServiceChanges(t *testing.T, s *service.Service) (func(), func()) {
	t.Helper()
	wait, release, ok := service.InspectForTest(s).Changes()
	if !ok {
		t.Fatal("cannot wait on a stopped service")
	}
	return func() { t.Helper(); fixturekit.Must(t, wait()) }, release
}

// StartContext starts a real service with the caller's cancellable lifetime.
func StartContext(ctx context.Context, opts service.Options, hooks service.TestHooks) (*service.Service, error) {
	if hooks.ReviewChecks == nil {
		hooks.ReviewChecks = fixturekit.PassingChecks
	}
	return service.StartForTest(ctx, opts, hooks)
}
