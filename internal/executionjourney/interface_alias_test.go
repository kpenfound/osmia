package executionjourney

import (
	api "github.com/kpenfound/osmia/internal/api"
	browsertest "github.com/kpenfound/osmia/internal/browsertest"
	delivery "github.com/kpenfound/osmia/internal/delivery"
	execution "github.com/kpenfound/osmia/internal/execution"
	intake "github.com/kpenfound/osmia/internal/intake"
	lifecycle "github.com/kpenfound/osmia/internal/lifecycle"
	service "github.com/kpenfound/osmia/internal/service"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	strings "strings"
	time "time"
)

const draftSubject = intake.DraftSubject

const amendmentPresented = intake.AmendmentPresented

const (
	designSessionSubject = intake.DesignSessionSubject
	designOpenValue      = intake.DesignOpenValue
	designClosedValue    = intake.DesignClosedValue
)

type WorkgroupChange = lifecycle.WorkgroupChange

type ConversationResponse = api.ConversationResponse

type chiefTurns = fixturekit.ChiefTurns

const draftInvalid = intake.DraftInvalid

const TurnDone = api.TurnDone

const cyclicPlan = fixturekit.CyclicPlan

const AmendmentApprove = intake.AmendmentApprove

const ownerRuling = intakefixture.OwnerRuling

type amendmentDrafter = intake.AmendmentDrafter

const TurnQueued = api.TurnQueued

type Event = api.Event

const InboxRatification = api.InboxRatification

var draftingActor = intake.DraftingActor

type ExtractionState = api.ExtractionState

const amendmentRuled = intake.AmendmentRuled

const EventInbox = api.EventInbox

const demoSecret = intakefixture.DemoSecret

var workstreamRecords = intakefixture.WorkstreamRecords

type SessionOpenRequest = api.SessionOpenRequest

var newBlockedFactoryOperation = fixturekit.NewBlockedFactoryOperation

const handedDocument = intake.HandedDocument

const demoRole = fixturekit.DemoRole

type fakeIssues = fixturekit.FakeIssues

var toolNames = intakefixture.ToolNames

const CharterEmpty = api.CharterEmpty

type AmendmentResponse = api.AmendmentResponse

type AgentStatus = api.AgentStatus

type Identity = api.Identity

const NoProject = api.NoProject

const deltaDraftTool = lifecycle.DeltaDraftTool

const AmendmentReject = intake.AmendmentReject

type AmendmentDecisionRequest = api.AmendmentDecisionRequest

var requestFactoryOperation = fixturekit.RequestFactoryOperation

const committeeRole = intakefixture.CommitteeRole

const SketchedState = intakefixture.SketchedState

type FeedEntry = api.FeedEntry

type draftStatus = intakefixture.DraftStatus

type Code = api.Code

type drafter = intake.Drafter

const RatifiedState = intake.RatifiedState

const demoAgent = intakefixture.DemoAgent

type AnswerResponse = api.AnswerResponse

const driftRequestSubject = lifecycle.DriftRequestSubject

type debate = intake.Debate

var readChanges = lifecycle.ReadChanges

const AbandonedState = intake.AbandonedState

type SessionDestroyResponse = api.SessionDestroyResponse

const NotFound = api.NotFound

type demoTurn = intakefixture.DemoTurn

type TraceCriterion = api.TraceCriterion

const Unavailable = api.Unavailable

var awaitEvents = fixturekit.AwaitEvents

const TurnFailed = api.TurnFailed

const architectAgent = intake.ArchitectAgent

type Architect = intake.Architect

const ActivityAgentTurn = api.ActivityAgentTurn

type HandInRequest = api.HandInRequest

type AmendmentDecision = api.AmendmentDecision

const InboxEscalation = api.InboxEscalation

var traceExec = fixturekit.TraceExec

var traceCommits = fixturekit.TraceCommits

const InboxAmendment = api.InboxAmendment

const Internal = api.Internal

var architectActor = intake.ArchitectActor

var request = fixturekit.Request

type ErrorResponse = api.ErrorResponse

const InShedState = intake.InShedState

const HandedState = intake.HandedState

var queueRoleTurn = fixturekit.QueueRoleTurn

const browserTimeout = browsertest.BrowserTimeout

type StatusView = api.StatusView

type ChangeRequest = lifecycle.ChangeRequest

type SendRequest = api.SendRequest

var traceFile = fixturekit.TraceFile

const Malformed = api.Malformed

type stagedWorkspaces = intakefixture.StagedWorkspaces

var assertGone = fixturekit.AssertGone

const squashStyle = delivery.SquashStyle

type WorkgroupView = api.WorkgroupView

type FeedSession = api.FeedSession

type notifyLedger = service.TestNotifyLedger

var checkSelectionPath = execution.CheckSelectionPath

var nextEvent = fixturekit.NextEvent

const coreReadBound = time.Second

type demoTransport = fixturekit.DemoTransport

var eventStream = fixturekit.EventStream

type ProjectAddRequest = api.ProjectAddRequest

const amendmentApproved = intake.AmendmentApproved

type TraceGap = api.TraceGap

var questionResult = intakefixture.QuestionResult

type sessionEngine = fixturekit.SessionEngine

var finalCheckAttemptPath = delivery.FinalCheckAttemptPath

type Diagnostic = api.Diagnostic

var recordDesignDraft = intakefixture.RecordDesignDraft

// syncBuffer is a bytes.Buffer safe for the concurrent writes a running
// service's background goroutines, such as the tailnet supervisor, make
// through its logger.
type syncBuffer = fixturekit.SyncBuffer

type FeedUnitChecks = api.FeedUnitChecks

type FeedTransition = api.FeedTransition

var (
	// amendedSpec changes the meaning of criterion 1 of validSpec.
	amendedSpec = strings.Replace(validSpec, "1. An interrupted upload resumes from the last acknowledged chunk.", "1. An interrupted upload resumes from a durable checkpoint.", 1)
)

var conversationEntries = service.TestConversationEntries

var amendmentDecisionID = intake.AmendmentDecisionID

var failed = intakefixture.Failed

var workstreamTraceDirectory = intakefixture.WorkstreamTraceDirectory

const architectRole = intakefixture.ArchitectRole

const Unsupported = api.Unsupported

const EventResync = api.EventResync

const EventWorkstream = api.EventWorkstream

var finalSystemPrompt = delivery.FinalSystemPrompt

const draftValid = intakefixture.DraftValid

type FeedFinalChecks = api.FeedFinalChecks

type InboxAnswer = api.InboxAnswer

const demoThread = intakefixture.DemoThread

var pauseOf = fixturekit.PauseOf

type StatusResponse = api.StatusResponse

const InboxDelivery = api.InboxDelivery

type SessionView = api.SessionView

const handedDesign = intakefixture.HandedDesign

var entitiesWith = intakefixture.EntitiesWith

type TraceEvent = api.TraceEvent

type InboxQuestion = api.InboxQuestion

type FeedCheckSelection = api.FeedCheckSelection

var assertCode = intakefixture.AssertCode

type FeedFinalReview = api.FeedFinalReview

type demoWorkspaces = fixturekit.DemoWorkspaces

var sketched = intakefixture.Sketched

var soon = intakefixture.Soon
