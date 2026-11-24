package lifecycle_test

import (
	slices "slices"
	testing "testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	config "github.com/kpenfound/osmia/internal/config"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func queue(f *journey.TwoProjectFixture, t *testing.T, repo *trace.Repository, id config.ProjectID, turn string) {
	t.Helper()
	fixturekit.QueueProjectTurn(t, repo, id, f.Streams[id], f.Clock.Now(), turn)
}

func dispatchedTurns(f *journey.TwoProjectFixture, t *testing.T, repo *trace.Repository, id config.ProjectID) []string {
	t.Helper()
	ops, err := repo.Operations(f.Streams[id])
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
