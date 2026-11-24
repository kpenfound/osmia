package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func ownPRFixture(t *testing.T) (*config.Config, *trace.Repository, *fixturekit.FakePulls, *baseRefresher) {
	t.Helper()
	_, cfg := conversationFixture(t, "own-pr-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	host := &fixturekit.FakePulls{}
	s := &Service{cfg: cfg, projects: []*activeProject{runtimeFor(repo)}, options: Options{Options: service.Options{PullRequests: host}}}
	return cfg, repo, host, &baseRefresher{s: s, repository: repo}
}

func sealStream(t *testing.T, repo *trace.Repository) {
	t.Helper()
	fixturekit.SealDependentStream(t, repo, fixturekit.Stream, fixturekit.Quiet, intake.FeatureBranch(fixturekit.Stream), service.TestForemanActor)
}

func deliverWithPullRequest(t *testing.T, repo *trace.Repository, cfg *config.Config, pr int) api.DeliveryPublication {
	t.Helper()
	return fixturekit.RecordDeliveredPublication(t, repo, cfg, fixturekit.Stream, intake.FeatureBranch(fixturekit.Stream), pr, service.TestForemanActor)
}
