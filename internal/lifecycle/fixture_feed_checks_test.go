package lifecycle_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func feedDocument(t *testing.T, repo *trace.Repository, ws config.WorkstreamID, id, path string, revision int, at time.Time, value any) {
	t.Helper()
	content, err := json.Marshal(value)
	must(t, err)
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: repo.Project(), Workstream: ws, At: at, Actor: serviceActor, Cause: "feed-test"}, Path: path, Content: string(content)}}))
}
