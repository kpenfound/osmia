package servicetest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// Project drives a registered local repository through a real service.
type Project struct {
	Options service.Options
	Hooks   service.TestHooks
	S       *service.Service
	C       *service.Client
	ID      config.ProjectID
	Clone   string
}

// NewProject starts a service with one local Git repository and a charter.
func NewProject(t *testing.T, configure ...func(*service.Options, *service.TestHooks)) *Project {
	t.Helper()
	opts, clone := ProjectFixture(t)
	home := filepath.Dir(clone)
	fixturekit.Must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", clone, "add", "internal")
	fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	fixturekit.DemoGit(t, home, "-C", clone, "branch", "-M", "main")
	hooks := service.TestHooks{}
	for _, prepare := range configure {
		prepare(&opts, &hooks)
	}
	s, c := Start(t, opts, hooks)
	added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	WriteCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	return &Project{Options: opts, Hooks: hooks, S: s, C: c, ID: added.Project.ID, Clone: clone}
}

// Repository returns the open trace; the fixture Must not close it while running.
func (f *Project) Repository(t *testing.T) *trace.Repository {
	t.Helper()
	repo, err := service.InspectForTest(f.S).Repository(f.ID)
	fixturekit.Must(t, err)
	return repo
}
func (f *Project) Stop(t *testing.T)  { t.Helper(); f.C.Close(); fixturekit.Must(t, f.S.Close()) }
func (f *Project) Start(t *testing.T) { t.Helper(); f.S, f.C = Start(t, f.Options, f.Hooks) }

// SeedBuild stops the service before seeding and returns the fixture-owned trace.
func (f *Project) SeedBuild(t *testing.T, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f.Stop(t)
	cfg, err := config.Load(f.Options.Config)
	fixturekit.Must(t, err)
	repo, err := trace.Open(cfg.Root, cfg.For(f.ID).Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	return f.recordBuild(t, repo, key, graph, backend, base), repo
}

func (f *Project) recordBuild(t *testing.T, repo *trace.Repository, key, graph, backend, base string) config.WorkstreamID {
	t.Helper()
	w, err := service.InspectForTest(f.S).PrepareBuild(context.Background(), repo, backend, base)
	fixturekit.Must(t, err)
	return fixturekit.RecordBuild(t, repo, fixturekit.BuildInput{Stream: w.Stream, At: w.At, Key: key, Graph: graph, Spec: fixturekit.ValidSpec, Branch: w.Branch, Workspace: w.Directory, Base: base, BaseBranch: w.BaseBranch})
}

// SeedBuilding publishes the sealed documents and graph together to live controllers.
func (f *Project) SeedBuilding(t *testing.T, key, graph string) config.WorkstreamID {
	t.Helper()
	repo := f.Repository(t)
	backend, err := service.InspectForTest(f.S).WorkspaceBackend(context.Background(), repo)
	fixturekit.Must(t, err)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	id := f.recordBuild(t, repo, key, graph, backend, base)
	fixturekit.Must(t, service.InspectForTest(f.S).RefreshRuntime())
	return id
}

// SeedBuildingPaused creates ready builds under an owner pause.
func (f *Project) SeedBuildingPaused(t *testing.T, target runtime.Target, graph string, keys ...string) []config.WorkstreamID {
	t.Helper()
	Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	var streams []config.WorkstreamID
	for _, key := range keys {
		streams = append(streams, f.SeedBuilding(t, key, graph))
	}
	return streams
}
