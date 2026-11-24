package journey

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func NewRebaseFixture(t *testing.T, key string) (*ShedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := NewMasonFixture(t, 1, intakefixture.IndependentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	stream, repository := SeedBuild(t, f, key, intakefixture.IndependentPlan, config.WorkspacesGit, base)
	intakefixture.PrepareRebase(t, f.UnitSeedDriver(repository), repository, stream, func(t *testing.T, r *trace.Repository, w config.WorkstreamID, u string, o *coreadapter.Outcome) trace.QueuedTurn {
		return CaptureMasonTurn(t, f, r, w, u, o)
	})
	return f, stream, repository
}

func RebaseOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []trace.OperationRecord {
	t.Helper()
	return intakefixture.UnitOperations(t, repository, stream, unit, func(op coreadapter.Operation) (string, error) {
		in, err := execution.DecodeRebase(op)
		return in.Unit, err
	})
}

func UnitRebases(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) []api.UnitRebase {
	t.Helper()
	return intakefixture.UnitRebases(t, repository, stream, unit, execution.RebaseDocument)
}

func CaptureMasonTurn(t *testing.T, f *ShedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	return CaptureTurn(t, f, repository, stream, intake.MasonAgent(unit), outcome)
}

func CaptureTurn(t *testing.T, f *ShedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	return f.SharedTurns().CaptureTurn(t, repository, stream, agent, outcome)
}
