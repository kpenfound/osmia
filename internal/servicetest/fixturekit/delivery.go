package fixturekit

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func RecordDeliveredPublication(t *testing.T, repo *trace.Repository, cfg *config.Config, stream config.WorkstreamID, branch string, pr int, actor trace.Actor) api.DeliveryPublication {
	t.Helper()
	ctx := context.Background()
	record := api.DeliveryPublication{Status: "opened", Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Base: cfg.Project.BaseBranch, Branch: branch, Commit: "feature-tip", PullRequest: pr, URL: fmt.Sprintf("https://example.invalid/pull/%d", pr)}
	data, err := json.Marshal(record)
	Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "final-publication", Revision: 1, Project: repo.Project(), Workstream: stream, At: DemoStart, Actor: actor, Cause: "fixture"}
	Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: "final/publication.json", Content: string(data)}}))
	h.Schema, h.ID = "osmia.trace.transition", "delivered"
	_, err = repo.SetFeatureState(ctx, h, "delivered", "delivered for fixture")
	Must(t, err)
	return record
}

func SealDependentStream(t *testing.T, repo *trace.Repository, stream, base config.WorkstreamID, branch string, actor trace.Actor) {
	t.Helper()
	ctx := context.Background()
	_, err := repo.SetWorkstreamBase(ctx, stream, base, 0, DemoStart)
	Must(t, err)
	sealed := seal.Seal{Version: 1, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash("spec"), Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "upstream-sha"}, Branch: branch, Workspace: "/owned/feature"}
	data, err := seal.Encode(sealed)
	Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: 1, Project: repo.Project(), Workstream: stream, At: DemoStart, Actor: actor, Cause: "seal"}
	Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: h, Path: seal.Path, Content: string(data)}}))
}
