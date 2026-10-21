package executionjourney

import (
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

type checkOutcome = fixturekit.CheckOutcome

var failedReport = fixturekit.FailedReport

type fakeChecks = fixturekit.FakeChecks

type fakePulls = fixturekit.FakePulls

const masonWrote = intakefixture.MasonWrote

const passedReport = fixturekit.PassedReport

var passing = fixturekit.Passing
