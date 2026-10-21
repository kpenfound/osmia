package service

import (
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// ownPRFixture opens a fresh trace repository holding workstreams stream
// and quiet, and a fake pull request host counting its own calls.
func ownPRFixture(t *testing.T) (*config.Config, *trace.Repository, *fakePulls, *fixtureBaseRefresher) {
	t.Helper()
	_, cfg := conversationFixture(t, "own-pr-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	host := &fakePulls{}
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{PullRequests: host}}
	return cfg, repo, host, &fixtureBaseRefresher{baseRefresher: &baseRefresher{S: s.lifecycle(), Repository: repo}, s: s}
}

type fixtureBaseRefresher struct {
	*baseRefresher
	s *Service
}

func stopFixture(t *testing.T) (*config.Config, *trace.Repository, *fakePulls, *fixtureBaseRefresher, *demoClock) {
	t.Helper()
	cfg, repo, host, clock := fixturekit.BaseRefreshTrace(t)
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{PullRequests: host, Reconciliation: reconcile.Options{Now: clock.Now}}}
	return cfg, repo, host, &fixtureBaseRefresher{baseRefresher: &baseRefresher{S: s.lifecycle(), Repository: repo}, s: s}, clock
}

func sealStream(t *testing.T, repo *trace.Repository) {
	t.Helper()
	fixturekit.SealDependentStream(t, repo, stream, quiet, featureBranch(stream), foremanActor)
}
func deliverWithPullRequest(t *testing.T, repo *trace.Repository, cfg *config.Config, pr int) DeliveryPublication {
	t.Helper()
	return fixturekit.RecordDeliveredPublication(t, repo, cfg, stream, featureBranch(stream), pr, foremanActor)
}
