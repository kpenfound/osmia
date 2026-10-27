package lifecycle

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

// nonSessionWorkstreams classifies sessions before releasing the guard that
// keeps their traces readable. The returned feature workstreams cannot be
// removed by session destroy while an observer processes them.
func nonSessionWorkstreams(repo *trace.Repository) ([]config.WorkstreamID, error) {
	var result []config.WorkstreamID
	err := repo.WithWorkstreamSnapshot(func() error {
		streams, err := repo.Workstreams()
		if err != nil {
			return err
		}
		for _, stream := range streams {
			if session, err := intake.IsSession(repo, stream); err != nil {
				return err
			} else if session {
				continue
			}
			if design, err := intake.IsDesignWorkstream(repo, stream); err != nil {
				return err
			} else if design {
				continue
			}
			result = append(result, stream)
		}
		return nil
	})
	return result, err
}

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
