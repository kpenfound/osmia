package fixturekit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

func BaseRefreshTrace(t *testing.T) (*config.Config, *trace.Repository, *FakePulls, *DemoClock) {
	t.Helper()
	_, cfg := Conversation(t, "base-stop-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	Must(t, err)
	t.Cleanup(func() { repo.Close() })
	return cfg, repo, &FakePulls{}, &DemoClock{At: DemoStart}
}
func OperationCount(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, action string) int {
	t.Helper()
	ops, err := repo.Operations(stream)
	Must(t, err)
	n := 0
	for _, o := range ops {
		if o.Operation.Action == action {
			n++
		}
	}
	return n
}

type baseIntegration struct {
	Base        seal.Base `json:"base"`
	Unavailable bool      `json:"unavailable,omitempty"`
}

func RecordBaseIntegration(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, at time.Time, commit string, actor trace.Actor) {
	t.Helper()
	obs := baseIntegration{Base: seal.Base{Remote: "origin", Branch: "main", Commit: commit}}
	data, err := json.Marshal(obs)
	Must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "base-observation", Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: actor, Cause: "fixture"}
	Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: "base-observation.json", Content: string(data)}}))
}
