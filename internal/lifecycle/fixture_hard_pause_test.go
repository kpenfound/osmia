package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// hardPauseRepository creates the demo project's trace with the workstream
// and n mason threads, agent-<i>, each with its turn t-<i> queued.
func hardPauseRepository(t *testing.T, n int) (*config.Config, *trace.Repository, *demoClock) {
	t.Helper()
	return fixturekit.HardPauseRepository(t, n)
}
