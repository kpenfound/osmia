package service

import (
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) sole() *activeProject {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.projects) == 0 {
		return nil
	}
	return s.projects[0]
}

func (s *Service) setSole(p *activeProject) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p == nil {
		s.projects = nil
		return
	}
	if p.id == "" {
		p.id = p.repository.Project()
	}
	s.projects = []*activeProject{p}
}

func runtimeFor(repository *trace.Repository) *activeProject {
	return &activeProject{id: repository.Project(), repository: repository}
}
