package fixturekit

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
	"testing"
	"time"
)

func FeedDocument(t *testing.T, repo *trace.Repository, ws config.WorkstreamID, id, path string, revision int, at time.Time, value any) {
	t.Helper()
	content, err := json.Marshal(value)
	Must(t, err)
	Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: repo.Project(), Workstream: ws, At: at, Actor: ServiceActor, Cause: "feed-test"}, Path: path, Content: string(content)}}))
}
