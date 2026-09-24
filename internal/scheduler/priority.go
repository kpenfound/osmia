package scheduler

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
)

// Rank returns each workstream's place in the project's runtime priority
// order, 1 for the first it names. Workstreams the order does not name, and
// every workstream of a project without an order, share the place after the
// last one it names. Orders of other projects are ignored.
func Rank(priorities []runtime.Priority, project config.ProjectID) func(config.WorkstreamID) int {
	rank := map[config.WorkstreamID]int{}
	for _, p := range priorities {
		if p.Project == project {
			for i, stream := range p.Workstreams {
				rank[stream] = i + 1
			}
		}
	}
	return func(stream config.WorkstreamID) int {
		if r, ok := rank[stream]; ok {
			return r
		}
		return len(rank) + 1
	}
}
