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

const otherProject config.ProjectID = "p_fedcba9876543210fedcba9876543210"

type twoProjectFixture struct {
	opts     journey.Options
	cfg      *config.Config
	clock    *fixturekit.DemoClock
	turns    *fixturekit.BlockingTurns
	streams  map[config.ProjectID]config.WorkstreamID
	scenario *fixturekit.TwoProjectScenario
}

func newTwoProjectFixture(t *testing.T) *twoProjectFixture {
	t.Helper()
	data := fixturekit.NewTwoProjectScenario(t)
	opts := journey.TestOptions(data.Config)
	opts.Workstreams, opts.Threads, opts.Reconciliation.Now = nil, data.Threads, data.Clock.Now
	return &twoProjectFixture{opts: opts, cfg: data.Loaded, clock: data.Clock, streams: data.Streams, turns: data.Turns, scenario: data}
}

func (f *twoProjectFixture) repository(id config.ProjectID) *trace.Repository {
	return f.scenario.Repository(id)
}

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
