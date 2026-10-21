package executionjourney

import (
	testing "testing"
	time "time"

	config "github.com/kpenfound/osmia/internal/config"

	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	trace "github.com/kpenfound/osmia/internal/trace"
)

var allTransitions = intakefixture.AllTransitions

const independentPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": [], "footprint": ["internal.trace"]}
]}
`

type transitionMove = intakefixture.TransitionMove

func (f *shedFixture) builtAs(t *testing.T, key string) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	return f.scenario().BuiltAs(t, key)
}

func (f *architectFixture) checkUnits(t *testing.T, stream config.WorkstreamID, want []UnitStatus) {
	t.Helper()
	intakefixture.CheckUnits(t, f.c, f.checkStatusError, stream, want)
}

func (f *architectFixture) checkStatusError(t *testing.T, err error, elapsed time.Duration) {
	t.Helper()
	intakefixture.CheckStatusError(t, intakefixture.StatusService{Done: f.s.done, Wait: f.s.Wait, Socket: f.s.Socket, Ready: f.s.ready.Load}, err, elapsed)
}
