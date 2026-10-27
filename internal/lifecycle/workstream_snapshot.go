package lifecycle

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

var nonSessionWorkstreams = intake.NonSessionWorkstreams

// snapshotWorkstreamBase protects the aggregate base read from concurrent
// session trace removal. It holds no guard during Git work or state writes.
func snapshotWorkstreamBase(repo *trace.Repository, stream config.WorkstreamID) (trace.WorkstreamBase, error) {
	var base trace.WorkstreamBase
	err := repo.WithWorkstreamSnapshot(func() error {
		var err error
		base, err = repo.WorkstreamBase(stream)
		return err
	})
	return base, err
}
