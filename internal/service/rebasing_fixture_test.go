package service

import (
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

func completeMasonTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit string, outcome string) trace.QueuedTurn {
	t.Helper()
	return f.sharedTurns().CompleteMasonTurn(t, repository, stream, unit, outcome)
}

func captureTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, agent string, outcome *coreadapter.Outcome) trace.QueuedTurn {
	t.Helper()
	return f.sharedTurns().CaptureTurn(t, repository, stream, agent, outcome)
}
