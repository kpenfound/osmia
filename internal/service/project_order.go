package service

import (
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// projectsByNewestWorkstream returns cfg's active project IDs ordered by the
// creation time of each project's newest workstream, newest first. Projects
// with equal newest times, and projects with no readable workstream creation
// time, keep their active_projects order, the latter after every other.
func (s *Service) projectsByNewestWorkstream(cfg *config.Config) []config.ProjectID {
	ids := cfg.ProjectIDs()
	if len(ids) < 2 {
		return ids
	}
	_, projects := s.runtimes()
	newest := make(map[config.ProjectID]time.Time, len(ids))
	for _, p := range projects {
		if p.repository == nil || !cfg.Active(p.id) {
			continue
		}
		if at, found, err := p.repository.NewestWorkstreamCreated(); err == nil && found {
			newest[p.id] = at
		}
	}
	slices.SortStableFunc(ids, func(a, b config.ProjectID) int {
		return newest[b].Compare(newest[a])
	})
	return ids
}
