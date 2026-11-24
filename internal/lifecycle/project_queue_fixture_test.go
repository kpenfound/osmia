package lifecycle_test

import (
	slices "slices"
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func (f *twoProjectFixture) queue(t *testing.T, repo *trace.Repository, id config.ProjectID, turn string) {
	t.Helper()
	fixturekit.QueueProjectTurn(t, repo, id, f.streams[id], f.clock.Now(), turn)
}

func (f *twoProjectFixture) dispatchedTurns(t *testing.T, repo *trace.Repository, id config.ProjectID) []string {
	t.Helper()
	ops, err := repo.Operations(f.streams[id])
	fixturekit.Must(t, err)
	var out []string
	for _, op := range ops {
		if in, err := thread.DecodeTurn(op.Operation); err == nil {
			out = append(out, in.Turn)
		}
	}
	slices.Sort(out)
	return out
}
