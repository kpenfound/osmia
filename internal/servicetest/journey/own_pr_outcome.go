package journey

import (
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func OwnPRFixture(t *testing.T) (*config.Config, *trace.Repository, *fixturekit.FakePulls, *BaseRefresher) {
	t.Helper()
	_, cfg := ConversationFixture(t, "own-pr-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	host := &fixturekit.FakePulls{}
	s := &Service{Cfg: cfg, Projects: []*ActiveProject{RuntimeFor(repo)}, Options: Options{Options: service.Options{PullRequests: host}}}
	return cfg, repo, host, &BaseRefresher{S: s, Repository: repo}
}

func SealStream(t *testing.T, repo *trace.Repository) {
	t.Helper()
	fixturekit.SealDependentStream(t, repo, fixturekit.Stream, fixturekit.Quiet, intake.FeatureBranch(fixturekit.Stream), execution.ForemanActor)
}

func DeliverWithPullRequest(t *testing.T, repo *trace.Repository, cfg *config.Config, pr int) api.DeliveryPublication {
	t.Helper()
	return fixturekit.RecordDeliveredPublication(t, repo, cfg, fixturekit.Stream, intake.FeatureBranch(fixturekit.Stream), pr, execution.ForemanActor)
}
