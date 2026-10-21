package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
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
	return intakefixture.SeedBuild(t, f.stop, f.s.current, func(t *testing.T, r *trace.Repository, k, g, b, base string) config.WorkstreamID {
		return recordBuild(t, f, r, k, g, b, base)
	}, key, graph, backend, base)
}

// seedBuilding gives a running service a sealed build ready for unit dispatch.
func (f *shedFixture) seedBuilding(t *testing.T, key, graph string) config.WorkstreamID {
	t.Helper()
	return intakefixture.SeedBuilding(t, f.clone, key, graph, func() (string, error) {
		b, err := f.s.newWorkspaces(context.Background(), f.s.about(f.repository()))
		return b.Backend, err
	}, func(k, g, b, base string) config.WorkstreamID {
		return recordBuild(t, f, f.repository(), k, g, b, base)
	}, f.s.refreshRuntime)
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
	intakefixture.SeedReview(t, repository, stream, unit, outcome, UnitReviewing, func(t *testing.T, r *trace.Repository, w config.WorkstreamID, u, o string) {
		seedChecking(t, f, r, w, u, o)
	}, func(t *testing.T, r *trace.Repository, w config.WorkstreamID) { runChecks(t, f.s, r, w) })
}

// seedChecking records a completed mason report and snapshots its
// candidate, so the unit is checking.
func seedChecking(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) {
	t.Helper()
	intakefixture.SeedChecking(t, f.unitSeedDriver(repository), repository, stream, unit, outcome)
}

func (f *shedFixture) unitSeedDriver(repository *trace.Repository) intakefixture.UnitSeedDriver[building] {
	m := newMasonController(f.s, repository)
	return intakefixture.UnitSeedDriver[building]{Read: m.read, Start: m.start, Finish: m.finish,
		Open: newUnitWorkspaces(f.s.current(), repository).open,
		Complete: func(t *testing.T, r *trace.Repository, w config.WorkstreamID, u, o string) trace.QueuedTurn {
			return completeMasonTurn(t, f, r, w, u, o)
		},
	}
}

func (f *shedFixture) buildFixture() intakefixture.BuildFixture {
	return intakefixture.BuildFixture{
		Upstream: f.upstream,
		Seed: func(t *testing.T, key, graph, backend, base string) (config.WorkstreamID, *trace.Repository) {
			return seedBuild(t, f, key, graph, backend, base)
		},
		Start: f.start,
	}
}
