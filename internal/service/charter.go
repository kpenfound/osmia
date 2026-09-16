package service

import (
	"context"
	"errors"
	"fmt"
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

// handIn checks that the project is active and its charter has rules. It
// reports the gate's failure or, past the gate, that hand-in is unsupported.
func (s *Service) handIn(ctx context.Context, req HandInRequest) *APIError {
	if err := config.CheckProjectIDs(req.Project); err != nil {
		return &APIError{Validation, "project must be a project ID: p_ followed by 32 lowercase hexadecimal digits"}
	}
	cfg := s.current()
	_, c, err := s.loadCharter(ctx, req.Project)
	if errors.Is(err, errNoActiveProject) {
		return &APIError{NotFound, fmt.Sprintf("project %s is not an active project; check the project ID with osmia status", req.Project)}
	}
	path := projectView(cfg.Root, config.Project{ID: req.Project}).Charter
	if errors.Is(err, errNoTrace) {
		return &APIError{Internal, fmt.Sprintf("project %s is configured but has no trace repository; register it with osmia project add", req.Project)}
	}
	if err != nil {
		return &APIError{Internal, fmt.Sprintf("cannot read or record the charter of project %s; check %s and the trace repository", req.Project, path)}
	}
	if c.Empty() {
		return &APIError{CharterEmpty, fmt.Sprintf("project %s cannot take work: its charter has no rules; write numbered rules (\"1. ...\") in %s", req.Project, path)}
	}
	return &APIError{Unsupported, fmt.Sprintf("project %s has a charter with %d rules, but hand-in is not implemented yet", req.Project, len(c.Rules))}
}
