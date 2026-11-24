package service

import (
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/osmia/internal/config"

	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

var (
	amendedSpec = strings.Replace(intakefixture.ValidSpec, "1. An interrupted upload resumes from the last acknowledged chunk.", "1. An interrupted upload resumes from a durable checkpoint.", 1)
	amendedPlan = strings.Replace(intakefixture.ValidPlan, "TestResume", "TestResumeAfterRestart", 1)
)

func builtForAmendment(t *testing.T, rounds int) (*shedFixture, config.WorkstreamID) {
	t.Helper()
	f := newDebateFixture(t, 1, rounds)
	return f, f.buildFixture().Amendment(t)
}

func presentAmendment(t *testing.T, f *shedFixture, stream config.WorkstreamID, role, spec, graph string, records ...shed.Record) string {
	t.Helper()
	return intakefixture.PresentAmendment(t, f.repository(), f.project, f.clock.Now(), stream, role, spec, graph, records...)
}

func (f *shedFixture) awaitAmendment(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	intakefixture.AwaitAmendment(t, f.repository(), stream, want)
}

func (f *shedFixture) revisions(t *testing.T, stream config.WorkstreamID, id string) []int {
	t.Helper()
	return f.scenario().DocumentRevisions(t, stream, id)
}

func (f *shedFixture) ruling(t *testing.T, stream config.WorkstreamID, requester string) []trace.QueuedTurn {
	t.Helper()
	return intakefixture.AmendmentRuling(t, f.repository(), stream, requester)
}
