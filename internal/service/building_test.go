package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// independentPlan is validPlan with no dependency between its units.
const independentPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": [], "footprint": ["internal.trace"]}
]}
`

var allTransitions = intakefixture.AllTransitions

// builtPaused builds one workstream for each key, as builtAs does, and sets a
// soft pause of the target once their packets wait for ratification, so no
// unit of them starts until the test clears it. A pause also holds the
// architect's drafts, so it is set after the hand-ins, when no unit exists.
func (f *shedFixture) builtPaused(t *testing.T, target runtime.Target, keys ...string) []config.WorkstreamID {
	t.Helper()
	return intakefixture.BuiltPaused(t, target, keys, f.shedAs, func() { mutation(t, f.c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Source: "owner"}) }, f.ratifiedBuild)
}

// shedAs hands in a workstream whose debate is skipped and waits in the
// shed for the owner's ratification of its packet.
func (f *shedFixture) shedAs(t *testing.T, key string) config.WorkstreamID {
	t.Helper()
	return f.scenario().ShedAs(t, key)
}

// ratifiedBuild ratifies the workstream's packet and returns its build
// operation once the workstream is building.
func (f *shedFixture) ratifiedBuild(t *testing.T, stream config.WorkstreamID) trace.OperationRecord {
	t.Helper()
	return f.scenario().RatifiedBuild(t, stream)
}

// checkUnits checks the unit states the status API lists for the workstream,
// alone and in the list.

func (f *shedFixture) builtAs(t *testing.T, key string) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	return f.scenario().BuiltAs(t, key)
}
