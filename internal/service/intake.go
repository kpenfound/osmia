package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

const AbandonedState = intake.AbandonedState

const ConcludedState = intake.ConcludedState

const DeliveredState = intake.DeliveredState

const abandonTransition = intake.AbandonTransition

const cancelReason = intake.CancelReason

var abandonActor = intake.AbandonActor

var abandoned = intake.Abandoned

const AmendmentRoundAction = intake.AmendmentRoundAction

const AmendmentReplyAction = intake.AmendmentReplyAction

type amendmentDebate = intake.AmendmentDebate

const AmendmentApprove = intake.AmendmentApprove

const AmendmentReject = intake.AmendmentReject

const AmendmentRound = intake.AmendmentRound

const AmendmentOverrule = intake.AmendmentOverrule

const amendmentRuled = intake.AmendmentRuled

func (s *Service) decideAmendment(ctx context.Context, raw string, id string, req AmendmentDecisionRequest) (AmendmentResponse, *APIError) {
	return s.intake().DecideAmendment(ctx, raw, id, req)
}

const decideAmendmentTool = intake.DecideAmendmentTool

const amendmentGuidance = intake.AmendmentGuidance

func (c *runtimeControls) heldGates(repository *trace.Repository, stream config.WorkstreamID) func() []trace.OwnerGate {
	return func() []trace.OwnerGate {
		s := c.service.Load()
		if s == nil {
			return nil
		}
		statuses, err := repository.Statuses()
		if err != nil {
			return nil
		}
		for _, w := range statuses {
			if w.Workstream == stream {
				return s.intake().HeldGates(repository, w)
			}
		}
		return nil
	}
}

func (c *runtimeControls) decideAmendment(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return c.intake().DecideAmendment(repository, scope)
}

const AmendmentDraftAction = intake.AmendmentDraftAction

type amendmentDrafter = intake.AmendmentDrafter

var amendmentSubject = intake.AmendmentSubject

const DraftAction = intake.DraftAction

const architectRole = intake.ArchitectRole

const architectAgent = intake.ArchitectAgent

const SketchedState = intake.SketchedState

const DraftTool = intake.DraftTool

const draftSubject = intake.DraftSubject

var architectActor = intake.ArchitectActor

type Architect = intake.Architect

type drafter = intake.Drafter

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

func (s *Service) decideCharter(ctx context.Context, raw string, question string, req CharterDecisionRequest) (CharterProposalView, *APIError) {
	return s.intake().DecideCharter(ctx, raw, question, req)
}

func (c *runtimeControls) decideCharter(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return c.intake().DecideCharter(repository, scope)
}

func (s *Service) agentProfile(cfg *config.Config, role string, agent string) (coreadapter.Profile, error) {
	return s.intake().AgentProfile(cfg, role, agent)
}

type apiErrAsError = intake.ApiErrAsError

var chiefActor = intake.ChiefActor

var ownerActor = intake.OwnerActor

var conversation = intake.Conversation

var queuedReason = intake.QueuedReason

const draftBriefToolName = intake.DraftBriefToolName

const designReadRepoToolName = intake.DesignReadRepoToolName

const designTitleToolName = intake.DesignTitleToolName

var isDesignWorkstream = intake.IsDesignWorkstream

func (s *Service) designRead(raw string) (api.DesignView, *APIError) {
	return s.intake().DesignRead(raw)
}

func (s *Service) startDesignWorkgroup(ctx context.Context, raw string, req ...api.DesignStartRequest) (api.DesignStartResponse, *APIError) {
	return s.intake().StartDesignWorkgroup(ctx, raw, req...)
}

const driftMasonAgent = intake.DriftMasonAgent

const driftReviewerAgent = intake.DriftReviewerAgent

const driftsDirectory = intake.DriftsDirectory

var revisionWorkersIdle = intake.RevisionWorkersIdle

var progress = intake.Progress

type stagedWorkspaces = intake.StagedWorkspaces

const factoryContextTool = intake.FactoryContextTool

const factoryPolicySubject = intake.FactoryPolicySubject

var recordFactoryPolicy = intake.RecordFactoryPolicy

const AssembledState = intake.AssembledState

const MaxHandedBytes = intake.MaxHandedBytes

const HandedState = intake.HandedState

var HandInWorkstream = intake.HandInWorkstream

func (s *Service) handIn(ctx context.Context, req HandInRequest) (HandInResponse, *APIError) {
	return s.intake().HandIn(ctx, req)
}

func (s *Service) inbox(ctx context.Context) (InboxResponse, *APIError) { return s.intake().Inbox(ctx) }

func (s *Service) crossRepoWorkgroupIDs(ctx context.Context, repository *trace.Repository) ([]config.WorkgroupID, error) {
	return s.intake().CrossRepoWorkgroupIDs(ctx, repository)
}

var decision = intake.Decision

func (s *Service) openDecisions(ctx context.Context, repository *trace.Repository, w trace.WorkstreamStatus) ([]InboxEntry, error) {
	return s.intake().OpenDecisions(ctx, repository, w)
}

func (s *Service) answer(ctx context.Context, raw string, req AnswerRequest) (api.AnswerResponse, *APIError) {
	return s.intake().Answer(ctx, raw, req)
}

var featureWorkspaces = intake.FeatureWorkspaces

var masonAgent = intake.MasonAgent

const resolveObjectionTool = intake.ResolveObjectionTool

var errPaused = intake.ErrPaused

var priorityRefusal = intake.PriorityRefusal

var registrationActor = intake.RegistrationActor

var serviceActor = intake.ServiceActor

func (s *Service) answers(cfg *config.Config, repository *trace.Repository) *questions.Deliverer {
	return s.intake().Answers(cfg, repository)
}

func (s *Service) packet(raw string) (api.PacketResponse, *APIError) { return s.intake().Packet(raw) }

func (s *Service) ratify(ctx context.Context, raw string, req RatifyRequest) (RatifyResponse, *APIError) {
	return s.intake().Ratify(ctx, raw, req)
}

var recoveryActor = intake.RecoveryActor

const readRemoteFileTool = intake.ReadRemoteFileTool

var sealedUnit = intake.SealedUnit

const reviewerRole = intake.ReviewerRole

var reviewerAgent = intake.ReviewerAgent

const SealAction = intake.SealAction

const RatifiedState = intake.RatifiedState

const branchesDirectory = intake.BranchesDirectory

var sealingActor = intake.SealingActor

var featureBranch = intake.FeatureBranch

type sealer = intake.Sealer

var latestRatification = intake.LatestRatification

var decodeSeal = intake.DecodeSeal

var unfinishedSessionTurn = intake.UnfinishedSessionTurn

const RoundAction = intake.RoundAction

const committeeRole = intake.CommitteeRole

const InShedState = intake.InShedState

const shedSubject = intake.ShedSubject

type Committee = intake.Committee

var committeeAgent = intake.CommitteeAgent

type debate = intake.Debate

var Dissent = intake.Dissent

type shedOwner = intake.ShedOwner

func (s *Service) shedAction(raw string, states ...string) (*shedOwner, *APIError) {
	return s.intake().ShedAction(raw, states...)
}

var featureState = intake.FeatureState

func (s *Service) shedRedraft(ctx context.Context, raw string, req ShedRedraftRequest) (ShedResponse, *APIError) {
	return s.intake().ShedRedraft(ctx, raw, req)
}

func (s *Service) shedMore(ctx context.Context, raw string, req ShedMoreRequest) (ShedResponse, *APIError) {
	return s.intake().ShedMore(ctx, raw, req)
}

const ReplyAction = intake.ReplyAction

const RedraftAction = intake.RedraftAction

type replier = intake.Replier

const masonRole = intake.MasonRole

const unitsDirectory = intake.UnitsDirectory

var errWorkgroupJoining = intake.ErrWorkgroupJoining

var workspaces = intake.Workspaces

type streamWorkspaces = intake.StreamWorkspaces

func (s *Service) intake() *intake.Controller {
	return intake.New(intake.Dependencies{
		BuildState: s.intakeBuildState, MasonBundle: s.intakeMasonBundle, BlockMason: s.intakeBlockMason, About: s.about,
		ApprovalTrace:            s.approvalTrace,
		Router:                   routerPort{s},
		ArchivedWorkstream:       s.archivedWorkstream,
		BaseEntry:                baseEntry,
		DiscoveryEntries:         discoveryEntries,
		RecoveryEntries:          s.recoveryEntries,
		ChangeHeld:               changeHeld,
		ChangeRepositoryHeld:     changeRepositoryHeld,
		ChiefContext:             chiefContext,
		ChiefOverride:            s.chiefOverride,
		DesignerOverride:         s.designerOverride,
		ChiefRepository:          s.chiefRepository,
		ChiefRepositoryForRead:   s.chiefRepositoryForRead,
		ContestedEntry:           s.contestedEntry,
		ContestedGate:            s.contestedGate,
		ContextFor:               s.contextFor,
		ContinueStopped:          s.continueStopped,
		ContractGuardRefusal:     s.contractGuardRefusal,
		ConversationTrace:        s.conversationTrace,
		CrossRepoMemberRepos:     s.crossRepoMemberRepos,
		Current:                  s.current,
		DeliveryDocuments:        deliveryDocuments,
		DeliveryEntry:            s.deliveryEntry,
		DeliveryGate:             s.deliveryGate,
		DeliveryRouteGate:        s.deliveryRouteGate,
		CancellationGates:        s.cancellationGates,
		DriftEntry:               driftEntry,
		Effective:                s.effective,
		FactoryContext:           factoryContext,
		ChecksChoiceEntry:        delivery.ChecksChoiceEntry,
		FinalChecksEntry:         delivery.FinalChecksEntry,
		FinalRoundsWithBlocking:  delivery.FinalRoundsWithBlocking,
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
		Reseeder:                 s.reseederOf,
		RemoteFileTool:           remoteFileTool,
		Repository:               s.repository,
		ProjectCreation: intake.ProjectCreation{Check: s.CheckProjectCreation, Reserve: s.ReserveProjectCreation,
			State: s.ProjectCreation, Create: s.CreateProject, Stuck: s.StuckProjectCreation},
		ResolveBase:           s.resolveBase,
		ResumeGroupApproval:   s.resumeGroupApproval,
		RoleExecution:         s.roleExecution,
		RuntimeOf:             s.intakeRuntimeOf,
		Runtimes:              s.intakeRuntimes,
		Step:                  s.step,
		Stoppable:             s.stoppable,
		StoreGeneration:       s.storeGeneration,
		ThreadRunner:          s.threadRunner,
		WorkgroupFullyRetired: s.workgroupFullyRetired,
		Store:                 func() *runtime.Store { return s.store }, HandInMu: &s.handInMu, Jev: s.jev, TrackTurn: s.turns.add,
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
