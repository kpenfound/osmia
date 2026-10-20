package service

import (
	"context"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) gitWorkgroupJoining(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	pending, err := repository.PendingCrossRepoWorkgroup(stream)
	if err != nil || pending != "" {
		return true, err
	}
	id, err := repository.CrossRepoWorkgroupOf(stream)
	if err != nil || id == "" {
		return false, err
	}
	_, group, _, err := repository.CrossRepoProjectSpec(id)
	if err != nil {
		return true, err
	}
	_, err = s.crossRepoMemberRepos(context.Background(), group)
	return err != nil, err
}

// gitOperation identifies the workflow effects sharing repository capacity.
func gitOperation(op coreadapter.Operation) bool {
	if op.Boundary != coreadapter.RepositoryBoundary {
		return false
	}
	switch op.Action {
	case SealAction, BuildAction, LandAction, RebaseAction, DriftAction, PublishAction, publishUpstreamAction:
		return true
	}
	return false
}

// gitOperationPool shares permits between workflow effects and remote polls.
// Counts survive stage reloads; a lower limit waits for existing work to drain.
type gitOperationPool struct {
	mu   sync.Mutex
	used map[config.ProjectID]int
}

func (p *gitOperationPool) try(project config.ProjectID, limit int, wake func()) (func(), bool) {
	if limit <= 0 {
		limit = config.DefaultGitOperations
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used[project] >= limit {
		return nil, false
	}
	if p.used == nil {
		p.used = map[config.ProjectID]int{}
	}
	p.used[project]++
	return sync.OnceFunc(func() {
		p.mu.Lock()
		p.used[project]--
		if p.used[project] == 0 {
			delete(p.used, project)
		}
		p.mu.Unlock()
		if wake != nil {
			wake()
		}
	}), true
}

// tryGitOperation admits repository work without waiting. Background polls
// use the same permits without taking a workstream workflow reservation.
func (s *Service) tryGitOperation(repository *trace.Repository) (func(), bool) {
	cfg := s.about(repository)
	return s.gitOperations.try(repository.Project(), cfg.Project.Capacity.GitOperations, repository.WakeWorkflow)
}

// gitOperationLimit composes configured admission with the shared repository
// limit. A rejected attempt returns every permit it acquired immediately.
func (s *Service) gitOperationLimit(repository *trace.Repository, other func(coreadapter.Operation) (func(), bool)) func(coreadapter.Operation) (func(), bool) {
	return func(op coreadapter.Operation) (func(), bool) {
		var releaseOther func()
		if other != nil {
			var admitted bool
			releaseOther, admitted = other(op)
			if !admitted {
				return nil, false
			}
		}
		if !gitOperation(op) {
			return releaseOther, true
		}
		// The controller wakes admission when an admitted attempt finishes.
		// Returning a permit after a busy workstream refused admission must
		// not wake the pass and repeatedly retry that same workstream.
		cfg := s.about(repository)
		release, admitted := s.gitOperations.try(repository.Project(), cfg.Project.Capacity.GitOperations, nil)
		if !admitted {
			if releaseOther != nil {
				releaseOther()
			}
			return nil, false
		}
		return sync.OnceFunc(func() {
			release()
			if releaseOther != nil {
				releaseOther()
			}
		}), true
	}
}
