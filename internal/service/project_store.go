package service

import (
	"errors"
	"maps"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
)

// The project store holds the registry and every project's settings. The
// service reads project settings only from it, and a registration, a removal
// and a settings save are the only writers while the service runs.

// settingsStore returns the project store of the loaded root. Its write steps
// pass through the service's step seam.
func (s *Service) settingsStore() *projectstore.Store {
	return projectstore.Open(s.current().Root, projectstore.WithStep(s.step))
}

// effectiveRevisions returns the number of the stored revision each project of
// cfg is loaded from: the current one, which is what a load of the store reads.
// A project the store holds no settings for, as before the import is complete,
// has none.
func effectiveRevisions(cfg *config.Config) (map[config.ProjectID]int, error) {
	store := projectstore.Open(cfg.Root)
	out := make(map[config.ProjectID]int, len(cfg.Projects))
	for _, id := range cfg.ProjectIDs() {
		revision, err := store.Current(id)
		if errors.Is(err, projectstore.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = revision.Number
	}
	return out, nil
}

// effectiveRevisionsNow returns a copy of the revisions the active projects'
// loaded settings come from.
func (s *Service) effectiveRevisionsNow() map[config.ProjectID]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.revisions)
}

// setEffective records the revision project id's loaded settings come from.
// The caller holds s.mu.
func (s *Service) setEffective(id config.ProjectID, number int) {
	if s.revisions == nil {
		s.revisions = map[config.ProjectID]int{}
	}
	s.revisions[id] = number
}

// withStoredProject returns cfg with project id, which cfg does not have,
// loaded from the current revision of its stored settings and added as the last
// active project, and the number of that revision. Stored settings that do not
// load are a *projectstore.LoadError.
func (s *Service) withStoredProject(cfg *config.Config, id config.ProjectID) (*config.Config, int, error) {
	file, revision, err := projectstore.Open(cfg.Root).Load(cfg, s.options.Config.Home, id)
	if err != nil {
		var field *config.FieldError
		if errors.As(err, &field) {
			err = &projectstore.LoadError{Project: id, Err: err}
		}
		return nil, 0, err
	}
	return cfg.WithLoadedProjects(append(slices.Clone(cfg.Projects), file.Project)), revision.Number, nil
}
