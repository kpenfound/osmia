package service

import (
	"errors"
	"os"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
)

// The project store holds the registry and every project's settings once the
// import is complete: a restart loads the projects from it. A registration, a
// removal and a settings save record their result there as well as in the
// files they write. Before the import is complete the files are all a restart
// reads, and nothing is recorded.

// recordRegistered records the registered project id, with the settings its
// file holds, in the store.
func (s *Service) recordRegistered(id config.ProjectID) error {
	root := s.current().Root
	store := projectstore.Open(root)
	if done, err := store.Imported(); err != nil || !done {
		return err
	}
	path, err := root.ProjectConfig(id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if _, err := store.WriteFirst(id, data); err != nil {
		return err
	}
	return store.Add(id)
}

// recordRemoved takes the removed project id off the store's registry.
func (s *Service) recordRemoved(id config.ProjectID) error {
	store := projectstore.Open(s.current().Root)
	if done, err := store.Imported(); err != nil || !done {
		return err
	}
	return store.Remove(id)
}

// recordSaved records the saved settings of project id, the bytes its file now
// holds, as the next revision in the store.
func (s *Service) recordSaved(id config.ProjectID, data []byte) error {
	store := projectstore.Open(s.current().Root)
	if done, err := store.Imported(); err != nil || !done {
		return err
	}
	current, err := store.Current(id)
	if errors.Is(err, projectstore.ErrNotFound) {
		_, err = store.WriteFirst(id, data)
		return err
	}
	if err != nil {
		return err
	}
	_, err = store.Replace(id, current.Pin, data)
	return err
}
