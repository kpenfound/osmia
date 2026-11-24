package service

import (
	"errors"
	"maps"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
)

func (s *Service) settingsStore() *projectstore.Store {
	return projectstore.Open(s.current().Root, projectstore.WithStep(s.step))
}

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

func (s *Service) effectiveRevisionsNow() map[config.ProjectID]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.revisions)
}

func (s *Service) setEffective(id config.ProjectID, number int) {
	if s.revisions == nil {
		s.revisions = map[config.ProjectID]int{}
	}
	s.revisions[id] = number
}

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
