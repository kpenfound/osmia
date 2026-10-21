package fixturekit

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// RecordActivity commits a transition record for the workstream at the given
// time. Its subject is unique to the record so it never conflicts with an
// earlier one.
func RecordActivity(t *testing.T, repo *trace.Repository, Stream config.WorkstreamID, id string, at time.Time) {
	t.Helper()
	_, err := repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: Project, Workstream: Stream, At: at, Actor: OwnerActor, Cause: "test-activity"},
		Subject: id, To: "done", Reason: "test activity",
	}})
	Must(t, err)
}
