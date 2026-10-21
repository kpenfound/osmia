package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

var failedReport = fixturekit.FailedReport

type fakePulls = fixturekit.FakePulls

const masonWrote = intakefixture.MasonWrote

type checkFunc = fixturekit.CheckFunc

var awaitAcknowledged = fixturekit.AwaitAcknowledged
var recordCharter = fixturekit.RecordCharter

const stream = fixturekit.Stream
const project = fixturekit.Project

var configureDeliverySigner = fixturekit.ConfigureDeliverySigner
