package fixturekit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func ResolutionDocument(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id, path string, value any, at time.Time, actor trace.Actor) {
	t.Helper()
	data, err := json.Marshal(value)
	Must(t, err)
	docs, err := trace.Read[trace.Document](repo, stream)
	Must(t, err)
	revision := 1
	for _, d := range docs {
		if d.ID == id && d.Revision >= revision {
			revision = d.Revision + 1
		}
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: repo.Project(), Workstream: stream, At: at, Actor: actor, Cause: "fixture"}
	Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: path, Content: string(data)}}))
}
