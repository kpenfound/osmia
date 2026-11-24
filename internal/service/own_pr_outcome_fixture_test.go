package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func ownPRFixture(t *testing.T) (*config.Config, *trace.Repository, *fixturekit.FakePulls, *fixtureBaseRefresher) {
	t.Helper()
	_, cfg := conversationFixture(t, "own-pr-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	intakefixture.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	host := &fixturekit.FakePulls{}
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{PullRequests: host}}
	return cfg, repo, host, &fixtureBaseRefresher{baseRefresher: &baseRefresher{S: s.lifecycle(), Repository: repo}, s: s}
}

type fixtureBaseRefresher struct {
	*baseRefresher
	s *Service
}

func stopFixture(t *testing.T) (*config.Config, *trace.Repository, *fixturekit.FakePulls, *fixtureBaseRefresher, *fixturekit.DemoClock) {
	t.Helper()
	cfg, repo, host, clock := fixturekit.BaseRefreshTrace(t)
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{PullRequests: host, Reconciliation: reconcile.Options{Now: clock.Now}}}
	return cfg, repo, host, &fixtureBaseRefresher{baseRefresher: &baseRefresher{S: s.lifecycle(), Repository: repo}, s: s}, clock
}
