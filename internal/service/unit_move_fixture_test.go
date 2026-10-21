package service

import (
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"

	"testing"
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
	scope := coreadapter.Scope{Project: string(c.f.project), Workstream: string(c.stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "events_1"}
	return &moveFixture{contestFixture: c, move: controls.moveUnit(c.repo, scope)}
}

func (m *moveFixture) moveUnit(t *testing.T, input string) string {
	t.Helper()
	return intakefixture.Handle(t, m.move, input)
}
