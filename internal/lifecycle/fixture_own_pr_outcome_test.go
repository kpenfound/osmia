package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// ownPRFixture opens a fresh trace repository holding workstreams stream
// and quiet, and a fake pull request host counting its own calls.
func ownPRFixture(t *testing.T) (*config.Config, *trace.Repository, *fakePulls, *baseRefresher) {
	t.Helper()
	_, cfg := conversationFixture(t, "own-pr-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	host := &fakePulls{}
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Options: service.Options{PullRequests: host}}}
	return cfg, repo, host, &baseRefresher{s: s, repository: repo}
}

// sealStream records stream sealed and depending on quiet, so Pass
// schedules its base-refresh.
func sealStream(t *testing.T, repo *trace.Repository) {
	t.Helper()
	fixturekit.SealDependentStream(t, repo, stream, quiet, featureBranch(stream), foremanActor)
}

// deliverWithPullRequest records stream delivered with one open pull
// request of its own, at the given number.
func deliverWithPullRequest(t *testing.T, repo *trace.Repository, cfg *config.Config, pr int) DeliveryPublication {
	t.Helper()
	return fixturekit.RecordDeliveredPublication(t, repo, cfg, stream, featureBranch(stream), pr, foremanActor)
}
