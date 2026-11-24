package lifecycle_test

import (
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func resolutionDocument(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id, path string, value any) {
	t.Helper()
	fixturekit.ResolutionDocument(t, repo, stream, id, path, value, fixturekit.DemoStart, intake.OwnerActor)
}

func recordResolutionMerge(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	resolutionDocument(t, repo, stream, delivery.MergeDocument, delivery.MergePath, api.DeliveryMerge{Branch: intake.FeatureBranch(stream), Commit: "merged-feature", Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: "merged-upstream"}})
}
