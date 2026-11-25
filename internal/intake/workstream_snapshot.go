package intake

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// Every place that iterates workstreams is classified once, here.
//
// Shared places read, account or notify and take every workstream that is not
// a session, whatever its kind: they list ObservedWorkstreams, or classify by
// IsSession alone. These are the event and question deliverers, the status,
// inbox and activity reads, and the pass that raises the unrecognised-kind
// notice.
//
// Kind-specific places move work: drafting, debate, sealing, building,
// scheduling a turn, landing, drift, delivery and the watchdogs. They list
// NonSessionWorkstreams, which leaves out a workstream whose stored kind this
// build does not register, so it waits and is never run as Feature, and they
// consult the kind's capability flags instead of assuming Feature.

// NonSessionWorkstreams classifies sessions before releasing the guard that
// keeps their traces readable. The returned workstreams are the ones this
// build runs: a workstream of an unregistered kind is left out. They cannot
// be removed by session destroy while an observer processes them.
func NonSessionWorkstreams(repo *trace.Repository) ([]config.WorkstreamID, error) {
	return discoverNonSessionWorkstreams(repo, nil, nil)
}

func discoverNonSessionWorkstreams(repo *trace.Repository, keepOpen func(config.WorkstreamID) (bool, error), listed func([]config.WorkstreamID) error) ([]config.WorkstreamID, error) {
	return discoverWorkstreams(repo, keepOpen, listed, false)
}

func discoverWorkstreams(repo *trace.Repository, keepOpen func(config.WorkstreamID) (bool, error), listed func([]config.WorkstreamID) error, unrecognised bool) ([]config.WorkstreamID, error) {
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
			if !unrecognised {
				if ok, err := KindRunnable(repo, stream); err != nil {
					return err
				} else if !ok {
					continue
				}
			}
			result = append(result, stream)
		}
		return nil
	})
	return result, err
}

func (s *Controller) designStartWorkstreams(repo *trace.Repository) ([]config.WorkstreamID, error) {
	return discoverNonSessionWorkstreams(repo, func(stream config.WorkstreamID) (bool, error) {
		return s.DesignStartPending(repo, stream)
	}, nil)
}
