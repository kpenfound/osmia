package lifecycle_test

import (
	config "github.com/kpenfound/osmia/internal/config"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	slices "slices"
	testing "testing"
)

func (f *twoProjectFixture) queue(t *testing.T, repo *trace.Repository, id config.ProjectID, turn string) {
	t.Helper()
	fixturekit.QueueProjectTurn(t, repo, id, f.streams[id], f.clock.Now(), turn)
}

// dispatchedTurns lists the turns the project's trace has turn operations for.
func (f *twoProjectFixture) dispatchedTurns(t *testing.T, repo *trace.Repository, id config.ProjectID) []string {
	t.Helper()
	ops, err := repo.Operations(f.streams[id])
	must(t, err)
	var out []string
	for _, op := range ops {
		if in, err := thread.DecodeTurn(op.Operation); err == nil {
			out = append(out, in.Turn)
		}
	}
	slices.Sort(out)
	return out
}
