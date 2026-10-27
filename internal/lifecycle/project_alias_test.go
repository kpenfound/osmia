package lifecycle_test

import (
	api "github.com/kpenfound/osmia/internal/api"
	browsertest "github.com/kpenfound/osmia/internal/browsertest"
	intake "github.com/kpenfound/osmia/internal/intake"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	service "github.com/kpenfound/osmia/internal/service"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

var projectContext = service.TestProjectContext

type ExtractionState = api.ExtractionState

const ProjectActive = api.ProjectActive

type PriorityRequest = api.PriorityRequest

type ProjectRuntime = api.ProjectRuntime

type ProfileRequest = api.ProfileRequest

const NoProject = api.NoProject

const RestartRequired = api.RestartRequired

var quote = browsertest.Quote

var factoryTarget = runtime.Target{Scope: "factory"}

const commentedConfig = fixturekit.CommentedConfig

const InboxEscalation = api.InboxEscalation

type completedRunner = fixturekit.CompletedRunner

var snapshot = intakefixture.Snapshot

const HandedState = intake.HandedState

// listenWeb is the configuration that opens the web listener on a free
// loopback port.
const listenWeb = "[listen]\nweb = \"127.0.0.1:0\"\n"

type DailyBudgetStatus = api.DailyBudgetStatus

type CapacityStatus = api.CapacityStatus

type RepoView = api.RepoView

var designSelectionOf = intake.DesignSelectionOf

var soon = intakefixture.Soon
