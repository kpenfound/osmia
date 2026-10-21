// Package servicetest starts and drives real services with fixture execution boundaries.
package servicetest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func Options(cfg config.Options) service.Options {
	return service.Options{Config: cfg, Build: api.Identity{Version: "test", Commit: "abc"}, Workstreams: []config.WorkstreamID{fixturekit.Stream}, ShutdownTimeout: 100 * time.Millisecond}
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
