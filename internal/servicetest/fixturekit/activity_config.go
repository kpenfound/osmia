package fixturekit

import (
	"context"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// activityFixture opens a fresh Project trace under its own home directory,
// without starting a service, so the caller can seed workstreams and
// activity before the first start.
func Activity(t *testing.T) (config.Options, *config.Config, *trace.Repository) {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "activity-")
	Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := ConfigAt(t, home)
	cfg, err := config.Load(opts)
	Must(t, err)
	Must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	DemoGit(t, home, "-C", cfg.Project.Clone, "init", "-q")
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, DemoStart, OwnerActor)
	Must(t, err)
	return opts, cfg, repo
}
