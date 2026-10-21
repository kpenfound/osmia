package servicetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestServiceFixtureSeedsBuildsAcrossRestart(t *testing.T) {
	t.Parallel()
	f := NewProject(t)
	f.Stop(t)
	WithWeb(t, f.Options, "127.0.0.1:0")
	passed := make(chan struct{}, 1)
	f.Hooks.SchedulePassed = func() {
		select {
		case passed <- struct{}{}:
		default:
		}
	}
	f.Start(t)
	if f.S.WebAddr() == "" {
		t.Fatal("fixture did not start the web listener")
	}
	select {
	case <-passed:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("scheduler never completed a pass")
	}
	graph := `{"version":1,"units":[{"id":"resume","title":"Resume uploads","addresses":[{"criterion":"spec#1","proof":{"kind":"new-test","name":"TestResume"}}],"depends_on":[],"footprint":[]}]}`
	streams := f.SeedBuildingPaused(t, runtime.Target{Scope: "factory"}, graph, "live")
	assertBuild := func(repo *trace.Repository, id config.WorkstreamID) {
		t.Helper()
		latest, _, found, err := seal.Latest(repo, id)
		fixturekit.Must(t, err)
		if !found || latest.SpecHash != seal.SpecHash(fixturekit.ValidSpec) {
			t.Fatalf("incomplete seal: %+v", latest)
		}
		state, err := repo.Workflow(id, trace.UnitSubject("resume"))
		fixturekit.Must(t, err)
		if state.Value != "ready" || state.Version != 2 {
			t.Fatalf("seeded unit: %+v", state)
		}
		view, err := f.C.Status(context.Background(), id)
		fixturekit.Must(t, err)
		if view.State == nil || *view.State != "building" || len(view.Units) != 1 || view.Units[0].State != "ready" {
			t.Fatalf("seeded API view: %+v", view)
		}
	}
	assertBuild(f.Repository(t), streams[0])
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	stopped, repo := f.SeedBuild(t, "stopped", graph, config.WorkspacesGit, base)
	fixturekit.Must(t, repo.Close())
	f.Start(t)
	assertBuild(f.Repository(t), streams[0])
	assertBuild(f.Repository(t), stopped)
}
