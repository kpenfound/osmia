package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// seedBuild records a sealed plan and builds its unit graph. It returns a
// stopped service's open repository for controller tests.
func seedBuild(t *testing.T, f *shedFixture, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f.stop(t)
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { repository.Close() })
	return recordBuild(t, f, repository, key, graph, backend, base), repository
}

// seedBuilding gives a running service a sealed build ready for unit dispatch.
func (f *shedFixture) seedBuilding(t *testing.T, key, graph string) config.WorkstreamID {
	t.Helper()
	backend, err := f.s.newWorkspaces(context.Background(), f.s.about(f.repository()))
	must(t, err)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream := recordBuild(t, f, f.repository(), key, graph, backend.Backend, base)
	must(t, f.s.refreshRuntime())
	return stream
}

// seedBuildingPaused creates ready builds under an owner pause.
func (f *shedFixture) seedBuildingPaused(t *testing.T, target runtime.Target, graph string, keys ...string) []config.WorkstreamID {
	t.Helper()
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	var streams []config.WorkstreamID
	for _, key := range keys {
		streams = append(streams, f.seedBuilding(t, key, graph))
	}
	return streams
}

// recordBuild publishes the sealed documents and unit states atomically so
// running controllers only see a complete build fixture.
func recordBuild(t *testing.T, f *shedFixture, repository *trace.Repository, key, graph, backend, base string) config.WorkstreamID {
	t.Helper()
	w, err := InspectForTest(f.s).PrepareBuild(context.Background(), repository, backend, base)
	must(t, err)
	return fixturekit.RecordBuild(t, repository, fixturekit.BuildInput{Stream: w.Stream, At: w.At, Key: key, Graph: graph, Spec: validSpec, Branch: w.Branch, Workspace: w.Directory, Base: base, BaseBranch: w.BaseBranch})
}

// newReviewFixture creates a unit whose completed report and candidate await review.
func newReviewFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedReview(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository
}

// seedReview records a completed mason report, snapshots its candidate and
// passes its checks, so the unit is reviewing.
func seedReview(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) {
	t.Helper()
	seedChecking(t, f, repository, stream, unit, outcome)
	runChecks(t, f.s, repository, stream)
	if state, err := repository.Workflow(stream, trace.UnitSubject(unit)); err != nil || state.Value != UnitReviewing {
		t.Fatalf("fixture unit %s is %s, not reviewing: %v", unit, state.Value, err)
	}
}

// seedChecking records a completed mason report and snapshots its
// candidate, so the unit is checking.
func seedChecking(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) {
	t.Helper()
	m := newMasonController(f.s, repository)
	ctx := context.Background()
	b, found, err := m.read(stream)
	must(t, err)
	if !found {
		t.Fatal("fixture workstream is not building")
	}
	started, blocked, err := m.start(ctx, b, unit)
	must(t, err)
	if !started || blocked {
		t.Fatalf("fixture unit %s did not start", unit)
	}
	w, _, err := newUnitWorkspaces(f.s.cfg, repository).open(ctx, stream, unit)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(w.Path, masonWrote), []byte("package trace\n"), 0644))
	completeMasonTurn(t, f, repository, stream, unit, outcome)
	b, _, err = m.read(stream)
	must(t, err)
	moved, blocked, err := m.finish(ctx, b, unit)
	must(t, err)
	if !moved || blocked {
		t.Fatalf("fixture unit %s did not enter checking", unit)
	}
}
