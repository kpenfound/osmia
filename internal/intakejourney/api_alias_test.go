package intakejourney

import (
	"github.com/kpenfound/osmia/internal/api"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

type AmendmentDecisionRequest = api.AmendmentDecisionRequest

type AmendmentResponse = api.AmendmentResponse

type UnitStatus = api.UnitStatus

type CharterDecisionRequest = api.CharterDecisionRequest

type CharterProposalView = api.CharterProposalView

type CharterProposalsResponse = api.CharterProposalsResponse

type ChiefContestDecision = api.ChiefContestDecision

type DeliveryPresentation = api.DeliveryPresentation

type DeliveryApproval = api.DeliveryApproval

type DeliveryDecision = api.DeliveryDecision

type CharterEdit = api.CharterEdit

type FinalReport = api.FinalReport

type FinalCriterion = api.FinalCriterion

type ContestedRulingRequest = api.ContestedRulingRequest

type ContestedRulingResponse = api.ContestedRulingResponse

const Prefix = api.Prefix

type Code = intakefixture.Code

const Validation = api.Validation

const Conflict = api.Conflict

const Unavailable = api.Unavailable

const Internal = api.Internal

const NoProject = api.NoProject

const NotFound = api.NotFound

const CharterEmpty = api.CharterEmpty

type APIError = intakefixture.APIError

type Diagnostic = intakefixture.Diagnostic

type RepoView = api.RepoView

type ExtractionState = api.ExtractionState

type CharterState = api.CharterState

type HandInRequest = api.HandInRequest

type HandInResponse = api.HandInResponse

type DesignView = api.DesignView

type DesignStartResponse = api.DesignStartResponse

type PacketResponse = api.PacketResponse

type RatifyRequest = api.RatifyRequest

type RatifyResponse = api.RatifyResponse

type PauseRequest = api.PauseRequest

type ClearPauseRequest = api.ClearPauseRequest

type PriorityRequest = api.PriorityRequest

type WorkspacesStatus = api.WorkspacesStatus

type WorkstreamStatus = api.WorkstreamStatus

type StatusView = api.StatusView

type InboxResponse = api.InboxResponse

const InboxEscalation = api.InboxEscalation

const InboxRatification = api.InboxRatification

const InboxContested = api.InboxContested

const InboxAmendment = api.InboxAmendment

const InboxDelivery = api.InboxDelivery

type InboxEntry = api.InboxEntry

type InboxAnswer = api.InboxAnswer

type InboxQuestion = api.InboxQuestion

type AnswerRequest = api.AnswerRequest

type AnswerResponse = api.AnswerResponse

const TurnDone = api.TurnDone

type ConversationResponse = intakefixture.ConversationResponse

type ConversationEntry = api.ConversationEntry

type FeedResponse = api.FeedResponse

type FeedEntry = api.FeedEntry

type DraftEdit = api.DraftEdit

type ProjectAddRequest = api.ProjectAddRequest
