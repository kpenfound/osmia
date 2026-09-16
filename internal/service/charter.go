package service

import (
	"context"
	"errors"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

var (
	errNoActiveProject = errors.New("no active project")
	errNoTrace         = errors.New("project has no trace")
)

// loadCharter returns the active project's charter through the trace, which
// records any owner edit first. Every service reader of the charter goes
// through it, so none sees an unrecorded edit.
func (s *Service) loadCharter(ctx context.Context, id config.ProjectID) (trace.Document, charter.Charter, error) {
	repository, err := s.repository(id)
	if err != nil {
		return trace.Document{}, charter.Charter{}, err
	}
	doc, err := repository.Charter(ctx, time.Now().UTC())
	if err != nil {
		return trace.Document{}, charter.Charter{}, err
	}
	return doc, charter.Parse(doc.Content), nil
}

// repository returns the open trace of the active project id.
func (s *Service) repository(id config.ProjectID) (*trace.Repository, error) {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	if !cfg.HasProject() || cfg.Project.ID != id {
		return nil, errNoActiveProject
	}
	if active == nil {
		return nil, errNoTrace
	}
	return active.repository, nil
}

// Context returns the provider that assembles turn bundles for the active
// project. Its charter reads record owner edits as loadCharter does.
func (s *Service) Context() bundle.Provider {
	return bundle.Files{Repository: s.repository, Now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) charterState(id config.ProjectID) (CharterState, error) {
	doc, c, err := s.loadCharter(context.Background(), id)
	if err != nil {
		return CharterState{}, err
	}
	return CharterState{Ready: !c.Empty(), Rules: len(c.Rules), Revision: doc.Revision, Diagnostics: c.Diagnostics}, nil
}
