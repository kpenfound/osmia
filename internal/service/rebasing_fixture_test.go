package service

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func moveFeature(t *testing.T, f *shedFixture, stream config.WorkstreamID, files map[string]string) string {
	t.Helper()
	return intakefixture.MoveFeature(t, workspaces(f.s.current(), branchesDirectory, backendOf(t, f, stream)), stream, files)
}

func rebaseOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []trace.OperationRecord {
	t.Helper()
	return intakefixture.UnitOperations(t, repository, stream, unit, func(op coreadapter.Operation) (string, error) { in, err := decodeRebase(op); return in.Unit, err })
}

func completeMasonTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) trace.QueuedTurn {
	t.Helper()
	return f.sharedTurns().CompleteMasonTurn(t, repository, stream, unit, outcome)
}

func captureTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	return f.sharedTurns().CaptureTurn(t, repository, stream, agent, outcome)
}

func newRebaseFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	intakefixture.PrepareRebase(t, f.unitSeedDriver(repository), repository, stream, func(t *testing.T, r *trace.Repository, w config.WorkstreamID, u string, o *coreadapter.Outcome) trace.QueuedTurn {
		return captureMasonTurn(t, f, r, w, u, o)
	})
	return f, stream, repository
}

func unitRebases(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []UnitRebase {
	t.Helper()
	return intakefixture.UnitRebases(t, repository, stream, unit, rebaseDocument)
}

func captureMasonTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	return captureTurn(t, f, repository, stream, masonAgent(unit), outcome)
}
