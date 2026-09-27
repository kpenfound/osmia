package scheduler

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
)

// Rank returns each workstream's place in the runtime priority order, 1 for
// the first it names. The order covers the workstreams of every project.
// Workstreams it does not name, and every workstream when there is no order,
// share the place after the last one it names.
func Rank(order []runtime.Ranked) func(config.ProjectID, config.WorkstreamID) int {
	rank := map[runtime.Ranked]int{}
	for i, r := range order {
		rank[r] = i + 1
	}
	return func(project config.ProjectID, stream config.WorkstreamID) int {
		if r, ok := rank[runtime.Ranked{Project: project, Workstream: stream}]; ok {
			return r
		}
		return len(rank) + 1
	}
}
