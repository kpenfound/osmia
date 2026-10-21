package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/trace"
)

// loadCharter returns the active project's latest charter revision.
func (s *Service) loadCharter(ctx context.Context, id config.ProjectID) (trace.Document, charter.Charter, error) {
	repository, err := s.repository(id)
	if err != nil {
		return trace.Document{}, charter.Charter{}, err
	}
	doc, err := repository.Charter(ctx)
	if err != nil {
		return trace.Document{}, charter.Charter{}, err
	}
	return doc, charter.Parse(doc.Content), nil
}

// repository returns the open trace of the active project id.
func (s *Service) repository(id config.ProjectID) (*trace.Repository, error) {
	cfg, active := s.runtimeOf(id)
	if !cfg.HasProject() {
		return nil, errNoActiveProject
	}
	if active == nil {
		return nil, errNoTrace
	}
	return active.repository, nil
}

// charterRepository also serves draining repos, whose turns still borrow the
// project's charter after the configuration stops admitting new work.
func (s *Service) charterRepository(id config.ProjectID) (*trace.Repository, error) {
	_, projects := s.runtimes()
	for _, p := range projects {
		if p.id == id && p.repository != nil {
			return p.repository, nil
		}
	}
	return nil, errNoTrace
}

// Context returns the provider that assembles turn bundles for the active
// project.
func (s *Service) Context() bundle.Provider {
	return hearsay.Provider{Local: bundle.Files{Repository: s.repository}, Config: s.current, Health: &s.memory}
}

func (s *Service) charterState(id config.ProjectID) (CharterState, error) {
	doc, c, err := s.loadCharter(context.Background(), id)
	if err != nil {
		return CharterState{}, err
	}
	return CharterState{Ready: !c.Empty(), Rules: len(c.Rules), Revision: doc.Revision, Diagnostics: c.Diagnostics}, nil
}

// contextFor keeps the trace and configuration available to draining operations.
func (s *Service) contextFor(repo *trace.Repository) bundle.Provider {
	return hearsay.Provider{Local: bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return repo, nil }}, Config: func() *config.Config { return s.about(repo) }, Health: &s.memory}
}
