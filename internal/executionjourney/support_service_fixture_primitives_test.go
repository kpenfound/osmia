package executionjourney

import (
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

type checkOutcome = fixturekit.CheckOutcome

var failedReport = fixturekit.FailedReport

type fakeChecks = fixturekit.FakeChecks

type fakePulls = fixturekit.FakePulls

const masonWrote = fixturekit.MasonWrote

const passedReport = fixturekit.PassedReport

var passing = fixturekit.Passing

type checkFunc = fixturekit.CheckFunc

var awaitAcknowledged = fixturekit.AwaitAcknowledged

var recordCharter = fixturekit.RecordCharter

const stream = fixturekit.Stream

const project = fixturekit.Project

var configureDeliverySigner = fixturekit.ConfigureDeliverySigner
