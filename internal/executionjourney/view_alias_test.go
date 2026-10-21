package executionjourney

import (
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
)

var factoryInstructions = service.TestFactoryInstructions
var ensureLibrarianThread = service.TestEnsureLibrarianThread
var feedEntries = service.TestFeedEntries
var feedSelection = service.TestFeedSelection
var statusView = service.TestStatusView

const librarianRole = service.TestLibrarianRole

var architectPrompt = intake.ArchitectPrompt
var questionDelivered = execution.QuestionDelivered
var checkBlockedSubject = execution.CheckBlockedSubject
var pendingMergeOrderReferences = lifecycle.PendingMergeOrderReferences
