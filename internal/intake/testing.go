package intake

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// NonSessionWorkstreamsForTest exposes the discovery boundary while the trace
// deletion guard is held, before any session classification reads.
func NonSessionWorkstreamsForTest(repo *trace.Repository, listed func([]config.WorkstreamID) error) ([]config.WorkstreamID, error) {
	return discoverNonSessionWorkstreams(repo, nil, listed)
}
