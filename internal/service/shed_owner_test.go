package service

import (
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	config "github.com/kpenfound/osmia/internal/config"

	testing "testing"
)

// ownerMoves lists the states the workstream's owner subject went through.
func (f *shedFixture) ownerMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	return intakefixture.OwnerMoves(t, f.transitions(t, stream))
}

var failed = intakefixture.Failed
