package intake

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// NonSessionWorkstreams classifies sessions before releasing the guard that
// keeps their traces readable. The returned feature workstreams cannot be
// removed by session destroy while an observer processes them.
func NonSessionWorkstreams(repo *trace.Repository) ([]config.WorkstreamID, error) {
	return discoverNonSessionWorkstreams(repo, nil, nil)
}

func discoverNonSessionWorkstreams(repo *trace.Repository, keepOpen func(config.WorkstreamID) (bool, error), listed func([]config.WorkstreamID) error) ([]config.WorkstreamID, error) {
	var result []config.WorkstreamID
	err := repo.WithWorkstreamDiscovery(func(streams []config.WorkstreamID) error {
		if listed != nil {
			if err := listed(streams); err != nil {
				return err
			}
		}
		for _, stream := range streams {
			if session, err := IsSession(repo, stream); err != nil {
				return err
			} else if session {
				continue
			}
			if design, err := IsDesignWorkstream(repo, stream); err != nil {
				return err
			} else if design {
				if keepOpen == nil {
					continue
				}
				keep, err := keepOpen(stream)
				if err != nil {
					return err
				}
				if !keep {
					continue
				}
			}
			result = append(result, stream)
		}
		return nil
	})
	return result, err
}

// designStartWorkstreams retains open designs only after the owner has
// durably started them. Those starts are fenced against session destruction.
func (s *Controller) designStartWorkstreams(repo *trace.Repository) ([]config.WorkstreamID, error) {
	return discoverNonSessionWorkstreams(repo, func(stream config.WorkstreamID) (bool, error) {
		return s.DesignStartPending(repo, stream)
	}, nil)
}
