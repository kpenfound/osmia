package service

import (
	"context"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/reconcile"
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
	group, _, err := repository.CrossRepoWorkgroup(id)
	if err != nil {
		return true, err
	}
	_, err = s.crossRepoMemberRepos(context.Background(), group)
	return err != nil, err
}

func gitOperation(op coreadapter.Operation) bool {
	if op.Boundary != coreadapter.RepositoryBoundary {
		return false
	}
	switch op.Action {
	case SealAction, BuildAction, LandAction, RebaseAction, DriftAction, PublishAction, publishUpstreamAction, landDeliveryAction:
		return true
	}
	return false
}

// GiveUpLimit is the failure streak that ends a landing, rebase or
// publication that keeps failing: the number of consecutive failures and how
// long after the first of them the operation is given up. The window is the
// configured `[loop] stall_after`, and the default of 45m while the stall
// watchdog is off. Code that explains an operation as in flight reads it, so an
// explanation lasts only while the operation can still be attempted.
func (s *Service) GiveUpLimit() reconcile.GiveUpLimit {
	return reconcile.NewGiveUpLimit(s.giveUpWindow())
}

func (s *Service) giveUpWindow() time.Duration {
	if w := s.options.Reconciliation.GiveUpWindow; w > 0 {
		return w
	}
	if cfg := s.current(); cfg != nil {
		return cfg.GiveUpWindow()
	}
	return 0
}

func networkOperation(op coreadapter.Operation) bool {
	return op.Boundary == coreadapter.RepositoryBoundary && op.Action == baseRefreshAction
}

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

func (s *Service) tryGitOperation(repository *trace.Repository) (func(), bool) {
	cfg := s.about(repository)
	return s.gitOperations.try(repository.Project(), cfg.Project.Capacity.GitOperations, repository.WakeWorkflow)
}

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
		if !gitOperation(op) && !networkOperation(op) {
			return releaseOther, true
		}
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
