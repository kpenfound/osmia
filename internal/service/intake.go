package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const AbandonedState = intake.AbandonedState

const DeliveredState = intake.DeliveredState

const abandonTransition = intake.AbandonTransition

const cancelReason = intake.CancelReason

var abandonActor = intake.AbandonActor

var abandoned = intake.Abandoned

const amendmentApplied = intake.AmendmentApplied

var amendmentMasonTurn = intake.AmendmentMasonTurn

const AmendmentRoundAction = intake.AmendmentRoundAction

const AmendmentReplyAction = intake.AmendmentReplyAction

type amendmentDebate = intake.AmendmentDebate

const AmendmentApprove = intake.AmendmentApprove

const AmendmentReject = intake.AmendmentReject

const AmendmentRound = intake.AmendmentRound

const AmendmentOverrule = intake.AmendmentOverrule

const amendmentPresented = intake.AmendmentPresented

const amendmentApproved = intake.AmendmentApproved

const amendmentRejected = intake.AmendmentRejected

const amendmentResealed = intake.AmendmentResealed

const amendmentUnapplied = intake.AmendmentUnapplied

const amendmentRuled = intake.AmendmentRuled

var amendmentActor = intake.AmendmentActor

var amendmentDecisionID = intake.AmendmentDecisionID

var amendmentRulingTurn = intake.AmendmentRulingTurn

func (s *Service) amendmentView(raw string, id string) (AmendmentResponse, *APIError) {
	return s.intake().AmendmentView(raw, id)
}

func (s *Service) decideAmendment(ctx context.Context, raw string, id string, req AmendmentDecisionRequest) (AmendmentResponse, *APIError) {
	return s.intake().DecideAmendment(ctx, raw, id, req)
}

func (s *Service) recordAmendmentDecision(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository, id string, req AmendmentDecisionRequest, actor trace.Actor, cause string) (AmendmentResponse, *APIError) {
	return s.intake().RecordAmendmentDecision(ctx, project, stream, repository, id, req, actor, cause)
}

const decideAmendmentTool = intake.DecideAmendmentTool

const amendmentGuidance = intake.AmendmentGuidance

func (c *runtimeControls) decideAmendment(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return c.intake().DecideAmendment(repository, scope)
}

const AmendmentDraftAction = intake.AmendmentDraftAction

type amendmentDrafter = intake.AmendmentDrafter

var amendmentSubject = intake.AmendmentSubject

type amendmentAffected = intake.AmendmentAffected

const DraftAction = intake.DraftAction

const architectRole = intake.ArchitectRole

const architectAgent = intake.ArchitectAgent

const architectThread = intake.ArchitectThread

const SketchedState = intake.SketchedState

const DraftTool = intake.DraftTool

const draftSubject = intake.DraftSubject

var architectActor = intake.ArchitectActor

var draftingActor = intake.DraftingActor

type Architect = intake.Architect

type drafter = intake.Drafter

const draftValid = intake.DraftValid

const draftInvalid = intake.DraftInvalid

type draftStatus = intake.DraftStatus

var draftNext = intake.DraftNext

var architectPrompt = intake.ArchitectPrompt

const BuildAction = intake.BuildAction

const BuildingState = intake.BuildingState

const UnitPlanned = intake.UnitPlanned

const UnitReady = intake.UnitReady

const UnitImplementing = intake.UnitImplementing

const UnitChecking = intake.UnitChecking

const UnitReviewing = intake.UnitReviewing

const UnitApproved = intake.UnitApproved

const UnitMerged = intake.UnitMerged

const UnitWaiting = intake.UnitWaiting

const UnitContested = intake.UnitContested

type builder = intake.Builder

var sealedPlan = intake.SealedPlan

var errNoActiveProject = intake.ErrNoActiveProject

var errNoTrace = intake.ErrNoTrace

func (s *Service) charterProposals() (CharterProposalsResponse, *APIError) {
	return s.intake().CharterProposals()
}

func (s *Service) charterProposal(raw string, question string) (CharterProposalView, *APIError) {
	return s.intake().CharterProposal(raw, question)
}

func (s *Service) decideCharter(ctx context.Context, raw string, question string, req CharterDecisionRequest) (CharterProposalView, *APIError) {
	return s.intake().DecideCharter(ctx, raw, question, req)
}

const decideCharterTool = intake.DecideCharterTool

const charterGuidance = intake.CharterGuidance

func (c *runtimeControls) decideCharter(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return c.intake().DecideCharter(repository, scope)
}

type charterer = intake.Charterer

const chiefDocumentsGuidance = intake.ChiefDocumentsGuidance

func (s *Service) agentProfile(cfg *config.Config, role string, agent string) (coreadapter.Profile, error) {
	return s.intake().AgentProfile(cfg, role, agent)
}

type apiErrAsError = intake.ApiErrAsError

var chiefActor = intake.ChiefActor

const contestGuidance = intake.ContestGuidance

const driftHandbackGuidance = intake.DriftHandbackGuidance

var ownerActor = intake.OwnerActor

const ownerWritingGuidance = intake.OwnerWritingGuidance

var conversation = intake.Conversation

var queuedReason = intake.QueuedReason

const draftBriefToolName = intake.DraftBriefToolName

const designReadRepoToolName = intake.DesignReadRepoToolName

const designStartTransition = intake.DesignStartTransition

var designLock = intake.DesignLock

var isDesignWorkstream = intake.IsDesignWorkstream

func (s *Service) openDesign(ctx context.Context, req DesignOpenRequest) (DesignView, *APIError) {
	return s.intake().OpenDesign(ctx, req)
}

func (s *Service) designList(rawProject string) (DesignsResponse, *APIError) {
	return s.intake().DesignList(rawProject)
}

func (s *Service) designRead(raw string) (DesignView, *APIError) { return s.intake().DesignRead(raw) }

func (s *Service) designMessage(ctx context.Context, raw string, req DesignMessageRequest) (ConversationEntry, *APIError) {
	return s.intake().DesignMessage(ctx, raw, req)
}

var draftBriefTool = intake.DraftBriefTool

var designReadRepoTool = intake.DesignReadRepoTool

var stageDesignWorkspace = intake.StageDesignWorkspace

func (s *Service) startDesignWorkgroup(ctx context.Context, raw string) (DesignStartResponse, *APIError) {
	return s.intake().StartDesignWorkgroup(ctx, raw)
}

const affectedReposTool = intake.AffectedReposTool

const designStartSubject = intake.DesignStartSubject

var designSelectionOf = intake.DesignSelectionOf

func (s *Service) designStartBlocksDraft(r *trace.Repository, stream config.WorkstreamID) (bool, error) {
	return s.intake().DesignStartBlocksDraft(r, stream)
}

func (s *Service) designStartPass(ctx context.Context, r *trace.Repository) error {
	return s.intake().DesignStartPass(ctx, r)
}

const driftMasonAgent = intake.DriftMasonAgent

const driftReviewerAgent = intake.DriftReviewerAgent

const driftsDirectory = intake.DriftsDirectory

const driftResolved = intake.DriftResolved

const driftRejected = intake.DriftRejected

var revisionWorkersIdle = intake.RevisionWorkersIdle

const librarianRole = intake.LibrarianRole

const librarianAgent = intake.LibrarianAgent

const librarianThread = intake.LibrarianThread

const maxExtractionAttempts = intake.MaxExtractionAttempts

var librarianWorkstream = intake.LibrarianWorkstream

var progress = intake.Progress

type stagedWorkspaces = intake.StagedWorkspaces

type noLease = intake.NoLease

var copyTracked = intake.CopyTracked

const factoryContextTool = intake.FactoryContextTool

const factoryPolicySubject = intake.FactoryPolicySubject

const factoryPolicyVersion = intake.FactoryPolicyVersion

const acceptanceDraftInstructions = intake.AcceptanceDraftInstructions

var recordFactoryPolicy = intake.RecordFactoryPolicy

const AssembledState = intake.AssembledState

const maxFinalRounds = intake.MaxFinalRounds

var nextRevision = intake.NextRevision

var groupApprovalResponse = intake.GroupApprovalResponse

const MaxHandedBytes = intake.MaxHandedBytes

const handedDocument = intake.HandedDocument

const HandedState = intake.HandedState

var handInKey = intake.HandInKey

var HandInWorkstream = intake.HandInWorkstream

func (s *Service) handIn(ctx context.Context, req HandInRequest) (HandInResponse, *APIError) {
	return s.intake().HandIn(ctx, req)
}

func (s *Service) inbox(ctx context.Context) (InboxResponse, *APIError) { return s.intake().Inbox(ctx) }

func (s *Service) crossRepoWorkgroupIDs(ctx context.Context, repository *trace.Repository) ([]config.WorkgroupID, error) {
	return s.intake().CrossRepoWorkgroupIDs(ctx, repository)
}

func (s *Service) workgroupInbox(ctx context.Context, known *trace.Repository, id config.WorkgroupID) ([]InboxEntry, *APIError) {
	return s.intake().WorkgroupInbox(ctx, known, id)
}

var decision = intake.Decision

func (s *Service) openDecisions(ctx context.Context, repository *trace.Repository, w trace.WorkstreamStatus) ([]InboxEntry, error) {
	return s.intake().OpenDecisions(ctx, repository, w)
}

func (s *Service) answer(ctx context.Context, raw string, req AnswerRequest) (AnswerResponse, *APIError) {
	return s.intake().Answer(ctx, raw, req)
}

var featureWorkspaces = intake.FeatureWorkspaces

var masonAgent = intake.MasonAgent

const resolveObjectionTool = intake.ResolveObjectionTool

const objectionGuidance = intake.ObjectionGuidance

func (c *runtimeControls) resolveObjection(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return c.intake().ResolveObjection(repository, scope)
}

var errPaused = intake.ErrPaused

const continueMark = intake.ContinueMark

var continuedTurn = intake.ContinuedTurn

var isContinuation = intake.IsContinuation

var priorityRefusal = intake.PriorityRefusal

var sliceMatchesPin = intake.SliceMatchesPin

var registrationActor = intake.RegistrationActor

var serviceActor = intake.ServiceActor

var groupMembers = intake.GroupMembers

func (s *Service) questionSignals(repository *trace.Repository) func(context.Context, trace.TurnRequest, coreadapter.Scope) string {
	return s.intake().QuestionSignals(repository)
}

func (s *Service) chiefEventsSkip(repository *trace.Repository) func(config.WorkstreamID) (bool, error) {
	return s.intake().ChiefEventsSkip(repository)
}

func (s *Service) chiefEventsPrompt(project config.ProjectID, repository *trace.Repository) func(context.Context, config.WorkstreamID) (string, error) {
	return s.intake().ChiefEventsPrompt(project, repository)
}

func (s *Service) answers(cfg *config.Config, repository *trace.Repository) *questions.Deliverer {
	return s.intake().Answers(cfg, repository)
}

func (s *Service) packet(raw string) (PacketResponse, *APIError) { return s.intake().Packet(raw) }

func (s *Service) ratify(ctx context.Context, raw string, req RatifyRequest) (RatifyResponse, *APIError) {
	return s.intake().Ratify(ctx, raw, req)
}

func (s *Service) ratifyMember(ctx context.Context, o *shedOwner, asked shed.Pin) (RatifyResponse, *APIError) {
	return s.intake().RatifyMember(ctx, o, asked)
}

const projectSpecCatchingUp = intake.ProjectSpecCatchingUp

var recoveryActor = intake.RecoveryActor

var copyCommit = intake.CopyCommit

const readRemoteFileTool = intake.ReadRemoteFileTool

var sealedUnit = intake.SealedUnit

const reviewerRole = intake.ReviewerRole

var reviewerAgent = intake.ReviewerAgent

const SealAction = intake.SealAction

const RatifiedState = intake.RatifiedState

const sealSubject = intake.SealSubject

const branchesDirectory = intake.BranchesDirectory

var sealingActor = intake.SealingActor

var featureBranch = intake.FeatureBranch

type sealer = intake.Sealer

var latestRatification = intake.LatestRatification

var decodeSeal = intake.DecodeSeal

var isSession = intake.IsSession

var chiefOfStaffSkip = intake.ChiefOfStaffSkip

var unfinishedSessionTurn = intake.UnfinishedSessionTurn

const RoundAction = intake.RoundAction

const committeeRole = intake.CommitteeRole

const InShedState = intake.InShedState

const shedSubject = intake.ShedSubject

type Committee = intake.Committee

var committeeAgent = intake.CommitteeAgent

var roundTurnPrefix = intake.RoundTurnPrefix

var roundTurnID = intake.RoundTurnID

type debate = intake.Debate

var Dissent = intake.Dissent

var latestPin = intake.LatestPin

type shedOwner = intake.ShedOwner

func (s *Service) shedAction(raw string, states ...string) (*shedOwner, *APIError) {
	return s.intake().ShedAction(raw, states...)
}

var featureState = intake.FeatureState

func (s *Service) shedObject(ctx context.Context, raw string, req ShedObjectRequest) (ShedResponse, *APIError) {
	return s.intake().ShedObject(ctx, raw, req)
}

func (s *Service) shedRule(ctx context.Context, raw string, req ShedRuleRequest) (ShedResponse, *APIError) {
	return s.intake().ShedRule(ctx, raw, req)
}

func (s *Service) shedOverrule(ctx context.Context, raw string, req ShedOverruleRequest) (ShedResponse, *APIError) {
	return s.intake().ShedOverrule(ctx, raw, req)
}

func (s *Service) shedRedraft(ctx context.Context, raw string, req ShedRedraftRequest) (ShedResponse, *APIError) {
	return s.intake().ShedRedraft(ctx, raw, req)
}

func (s *Service) shedSkip(ctx context.Context, raw string) (ShedResponse, *APIError) {
	return s.intake().ShedSkip(ctx, raw)
}

func (s *Service) shedMore(ctx context.Context, raw string, req ShedMoreRequest) (ShedResponse, *APIError) {
	return s.intake().ShedMore(ctx, raw, req)
}

const ReplyAction = intake.ReplyAction

const RedraftAction = intake.RedraftAction

var replyTurnID = intake.ReplyTurnID

type replier = intake.Replier

const masonRole = intake.MasonRole

const unitsDirectory = intake.UnitsDirectory

var errChangeBoundary = intake.ErrChangeBoundary

var queuedTurn = intake.QueuedTurn

var errWorkgroupJoining = intake.ErrWorkgroupJoining

var workspaces = intake.Workspaces

type streamWorkspaces = intake.StreamWorkspaces

func (s *Service) intake() *intake.Controller {
	return intake.New(intake.Dependencies{
		BuildState: s.intakeBuildState, MasonBundle: s.intakeMasonBundle, BlockMason: s.intakeBlockMason, About: s.about,
		ApprovalTrace:            s.approvalTrace,
		ArchivedWorkstream:       s.archivedWorkstream,
		BaseEntry:                baseEntry,
		ChangeHeld:               changeHeld,
		ChangeRepositoryHeld:     changeRepositoryHeld,
		ChiefContext:             chiefContext,
		ChiefOverride:            s.chiefOverride,
		ChiefRepository:          s.chiefRepository,
		ContestedEntry:           s.contestedEntry,
		ContextFor:               s.contextFor,
		ContinueStopped:          s.continueStopped,
		ContractGuardRefusal:     s.contractGuardRefusal,
		ConversationTrace:        s.conversationTrace,
		CrossRepoMemberRepos:     s.crossRepoMemberRepos,
		Current:                  s.current,
		DeliveryDocuments:        deliveryDocuments,
		DeliveryEntry:            s.deliveryEntry,
		DriftEntry:               driftEntry,
		Effective:                s.effective,
		FactoryContext:           factoryContext,
		FinalChecksEntry:         finalChecksEntry,
		FinalRoundsWithBlocking:  finalRoundsWithBlocking,
		FirstLiveWorkgroupMember: s.firstLiveWorkgroupMember,
		GroupApprovalLock:        s.groupApprovalLock,
		Held:                     s.held,
		InspectCode:              inspectCode,
		LatestFinalReport:        latestFinalReport,
		LoadCharter:              s.loadCharter,
		Log:                      s.log,
		MemberArchives:           s.memberArchives,
		MemoryTurns:              memoryTurns,
		NewWorkspaces:            s.newWorkspaces,
		Now:                      s.now,
		Pausing:                  s.pausing,
		PrepareGroupApproval:     s.prepareGroupApproval,
		ProjectFor:               s.projectFor,
		PublicationEntry:         s.publicationEntry,
		RefreshRuntime:           s.refreshRuntime,
		RemoteFileTool:           remoteFileTool,
		Repository:               s.repository,
		ResolveBase:              s.resolveBase,
		ResumeGroupApproval:      s.resumeGroupApproval,
		RoleExecution:            s.roleExecution,
		RuntimeOf:                s.intakeRuntimeOf,
		Runtimes:                 s.intakeRuntimes,
		Step:                     s.step,
		Stoppable:                s.stoppable,
		StoreGeneration:          s.storeGeneration,
		ThreadRunner:             s.threadRunner,
		WorkgroupFullyRetired:    s.workgroupFullyRetired,
		Store:                    func() *runtime.Store { return s.store }, HandInMu: &s.handInMu, Jev: s.jev, TrackTurn: s.turns.add,
		Options: intake.Execution{Architect: s.options.Architect, Committee: s.options.Committee, Issues: s.options.Issues, Controls: s.options.controls.intake()}})
}

func (c *runtimeControls) intake() *intake.Controls {
	return &intake.Controls{Load: func() *intake.Controller {
		if c == nil {
			return nil
		}
		s := c.service.Load()
		if s == nil {
			return nil
		}
		return s.intake()
	}}
}

func (s *Service) intakeRuntimeOf(id config.ProjectID) (*config.Config, *intake.Project) {
	cfg, active := s.runtimeOf(id)
	if active == nil {
		return cfg, nil
	}
	return cfg, &intake.Project{ID: active.id, Repository: active.repository}
}
func (s *Service) intakeRuntimes() (*config.Config, []*intake.Project) {
	cfg, projects := s.runtimes()
	out := make([]*intake.Project, 0, len(projects))
	for _, p := range projects {
		out = append(out, &intake.Project{ID: p.id, Repository: p.repository})
	}
	return cfg, out
}
func (s *Service) intakeBuildState(repo *trace.Repository, stream config.WorkstreamID) (intake.BuildState, bool, error) {
	b, found, err := (&masons{s: s, cfg: s.about(repo), repository: repo}).read(stream)
	return intake.BuildState{States: b.States(), Plan: b.Plan()}, found, err
}
func (s *Service) intakeMasonBundle(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit string) (bundle.Mason, error) {
	return (&masons{s: s, cfg: s.about(repo), repository: repo}).bundle(ctx, stream, unit)
}
func (s *Service) intakeBlockMason(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, unit, cause, reason string) error {
	return (&masons{s: s, cfg: s.about(repo), repository: repo}).block(ctx, stream, unit, cause, reason)
}
