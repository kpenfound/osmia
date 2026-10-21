package service

import (
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	config "github.com/kpenfound/osmia/internal/config"

	strings "strings"
	testing "testing"

	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

var (
	// amendedSpec changes the meaning of criterion 1 of validSpec.
	amendedSpec = strings.Replace(validSpec, "1. An interrupted upload resumes from the last acknowledged chunk.", "1. An interrupted upload resumes from a durable checkpoint.", 1)
	// amendedPlan renames the proof of unit resume in validPlan.
	amendedPlan = strings.Replace(validPlan, "TestResume", "TestResumeAfterRestart", 1)
)

// builtForAmendment returns a fixture with a building workstream on seal 1,
// a committee of one and the given shed.max_rounds.
func builtForAmendment(t *testing.T, rounds int) (*shedFixture, config.WorkstreamID) {
	t.Helper()
	f := newDebateFixture(t, 1, rounds)
	return f, f.buildFixture().Amendment(t)
}

// presentAmendment plants amendment 1 as the chief of staff presents it: the
// requester of the given role asked for it from its turn filing on unit
// resume, which waits for it, the architect proposed spec and graph, the
// given committee records make its first round, and packet.json revision 1
// is presented. It returns the requester's agent.
func presentAmendment(t *testing.T, f *shedFixture, stream config.WorkstreamID, role, spec, graph string, records ...shed.Record) string {
	t.Helper()
	return intakefixture.PresentAmendment(t, f.repository(), f.project, f.clock.Now(), stream, role, spec, graph, records...)
}

// awaitAmendment waits until amendment 1 reaches the wanted state.
func (f *shedFixture) awaitAmendment(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	intakefixture.AwaitAmendment(t, f.repository(), stream, want)
}

// revisions lists the recorded revisions of one document.
func (f *shedFixture) revisions(t *testing.T, stream config.WorkstreamID, id string) []int {
	t.Helper()
	return f.scenario().DocumentRevisions(t, stream, id)
}

// ruling returns the turns of the requester's thread that deliver the
// ruling on amendment 1.
func (f *shedFixture) ruling(t *testing.T, stream config.WorkstreamID, requester string) []trace.QueuedTurn {
	t.Helper()
	return intakefixture.AmendmentRuling(t, f.repository(), stream, requester)
}
