package intakejourney

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/trace"
)

func drivePass(t *testing.T, s *journey.Service, repo *trace.Repository, pass func() error) {
	t.Helper()
	intakefixture.DrivePass(t, repo, func() error { return s.Halt(s.Sole()) }, pass)
}

func plantReport(t *testing.T, r *trace.Repository, stream config.WorkstreamID, unit string, at time.Time) {
	t.Helper()
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.UnitSubject(unit) + "-report", Revision: 1, Project: r.Project(), Workstream: stream, Unit: unit, At: at, Actor: intake.MasonActor, Cause: "planted"}
	fixturekit.Must(t, r.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: "units/" + unit + "/report.json",
		Content: `{"unit":"` + unit + `","turn":"mason-` + unit + `-implement","seal":1,"outcome":"done","branch":"unit/` + unit + `","base":"base","candidate":"candidate"}` + "\n"}}))
}
