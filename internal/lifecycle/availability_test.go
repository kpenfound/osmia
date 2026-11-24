package lifecycle_test

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestAvailabilityPassesPreserveTraceErrors(t *testing.T) {
	t.Parallel()
	for _, subject := range []string{"base", "edge"} {
		t.Run(subject, func(t *testing.T) {
			t.Parallel()
			f := newEdgeGraphFixture(t, trace.EdgeHardBlock)
			id, path := "workstream-base", "base.json"
			if subject == "edge" {
				id, path = lifecycle.EdgeObservationDocument, lifecycle.EdgeObservationPath
			}
			h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: f.project, Workstream: f.downstream, At: f.s.now(), Actor: intake.OwnerActor, Cause: "fixture"}
			fixturekit.Must(t, f.repository.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: path, Content: "{"}}))
			var err error
			if subject == "base" {
				err = f.s.lifecycle().BaseWaitPass(context.Background(), f.repository)
			} else {
				err = f.s.lifecycle().EdgeWaitPass(context.Background(), f.repository)
			}
			if err == nil {
				t.Fatal("corrupt live trace did not fail the scheduling pass")
			}
		})
	}
}
