package service

import (
	config "github.com/kpenfound/osmia/internal/config"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// twoProjectFixture is a root with two active projects that share one mason
// slot, each with its own trace, workstream and mason thread.
type twoProjectFixture struct {
	opts     Options
	cfg      *config.Config
	clock    *demoClock
	turns    *blockingTurns
	streams  map[config.ProjectID]config.WorkstreamID
	scenario *fixturekit.TwoProjectScenario
}
