package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

type moveFixture struct {
	*contestFixture
	move coreadapter.Tool
}

func newMoveFixture(t *testing.T, key string) *moveFixture {
	t.Helper()
	c := newContestFixture(t, key)
	controls := &runtimeControls{}
	controls.service.Store(c.f.s)
	scope := intakefixture.ManagerEventScope(c.f.project, c.stream)
	return &moveFixture{contestFixture: c, move: controls.moveUnit(c.repo, scope)}
}
