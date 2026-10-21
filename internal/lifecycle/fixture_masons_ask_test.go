package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func (f *shedFixture) unitState(stream config.WorkstreamID, unit string) (string, error) {
	states, err := f.repository().WorkflowStates(stream)
	return states[trace.UnitSubject(unit)].Value, err
}
