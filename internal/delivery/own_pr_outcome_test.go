package delivery

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestOwnPullRequestOutcomeIgnoresAnEarlierPullRequestNumber(t *testing.T) {
	t.Parallel()
	cfg, repo := deliveryTrace(t)
	deliverWithPullRequest(t, repo, cfg, 7)
	fixturekit.Must(t, RecordOwnPullRequestOutcome(context.Background(), repo, fixturekit.DemoStart, stream, "fixture", 3, OwnPullRequestOutcomeClosed))
	finished, err := OwnPullRequestFinished(repo, stream)
	fixturekit.Must(t, err)
	if finished {
		t.Fatal("an outcome recorded for an earlier pull request counted for the workstream's current pull request")
	}
}
func deliverWithPullRequest(t *testing.T, repo *trace.Repository, cfg *config.Config, pr int) DeliveryPublication {
	t.Helper()
	return fixturekit.RecordDeliveredPublication(t, repo, cfg, stream, featureBranch(stream), pr, foremanActor)
}
