package service

import (
	testing "testing"

	config "github.com/kpenfound/osmia/internal/config"

	trace "github.com/kpenfound/osmia/internal/trace"
)

func newAskingMasonFixturePrepared(t *testing.T, masons int, drafted string, p *faults, prepare func(*Options)) (*shedFixture, *fakeMasons, *chief) {
	t.Helper()
	f, fake := newMasonFixturePrepared(t, masons, drafted, prepare)
	c := &chief{P: p, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.engine.Mu.Lock()
	defer f.engine.Mu.Unlock()
	f.engine.Turns["*"] = c.Turn
	return f, fake, c
}

func (f *shedFixture) unitState(stream config.WorkstreamID, unit string) (string, error) {
	states, err := f.repository().WorkflowStates(stream)
	return states[trace.UnitSubject(unit)].Value, err
}
