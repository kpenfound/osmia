package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// TestSharedTestHelpersIsolateStatePerCaller runs two subtests in parallel,
// each building its own fixture, trace repository, and service through the
// package's shared test helpers (fixture, demoGit, start, must) on its own
// root, the way conversationFixture does, and recording its own workstream.
// It asserts that each subtest's repository sees only the workstream it
// recorded, showing that these helpers share no mutable state between
// concurrent callers.
func TestSharedTestHelpersIsolateStatePerCaller(t *testing.T) {
	t.Parallel()
	caller := func(label string) func(t *testing.T) {
		return func(t *testing.T) {
			t.Parallel()
			opts := fixture(t)
			cfg, err := config.Load(opts.Config)
			must(t, err)
			must(t, os.MkdirAll(cfg.Project.Clone, 0700))
			demoGit(t, filepath.Dir(cfg.Project.Clone), "-C", cfg.Project.Clone, "init", "-q")
			created, err := trace.Create(context.Background(), cfg.Root, cfg.Project, time.Now().UTC(), ownerActor)
			must(t, err)
			must(t, created.Close())

			s, _ := start(t, opts)
			repository := s.sole().repository

			own, err := config.NewWorkstreamID()
			must(t, err)
			must(t, repository.CreateWorkstream(context.Background(), own, time.Now().UTC(), ownerActor))

			streams, err := repository.Workstreams()
			must(t, err)
			if len(streams) != 1 || streams[0] != own {
				t.Fatalf("%s: workstreams %v, want only %s", label, streams, own)
			}
		}
	}
	t.Run("alpha", caller("alpha"))
	t.Run("beta", caller("beta"))
}
