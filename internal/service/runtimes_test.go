package service

import "github.com/kpenfound/osmia/internal/trace"

// sole returns the runtime of the service's only open project, or nil.
func (s *Service) sole() *activeProject {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.projects) == 0 {
		return nil
	}
	return s.projects[0]
}

// setSole makes p the service's only open project runtime, or leaves none
// when p is nil.
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

// runtimeFor is the runtime of an open trace, as the service keeps it.
func runtimeFor(repository *trace.Repository) *activeProject {
	return &activeProject{id: repository.Project(), repository: repository}
}
