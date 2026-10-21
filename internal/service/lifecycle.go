package service

import (
	context "context"
	time "time"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	lifecycle "github.com/kpenfound/osmia/internal/lifecycle"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	seal "github.com/kpenfound/osmia/internal/seal"
	trace "github.com/kpenfound/osmia/internal/trace"
	workspace "github.com/kpenfound/osmia/internal/workspace"
)

func (s *Service) lifecycle() *lifecycle.Controller {
	return lifecycle.New(lifecycle.Dependencies{
		PollTask:                  s.pollTask,
		PollCursor:                pollCursor,
		ObserveSharedConversation: s.observeSharedConversation,
		Execution:                 s.execution,
		Intake:                    s.intake,
		StartActivity:             s.StartActivity,
		Abandon:                   s.abandon,
		About:                     s.about,
		ApprovalTrace:             s.approvalTrace,
		ConversationTrace:         s.conversationTrace,
		Current:                   s.current,
		DriftAsked:                &s.driftAsked,
		Effective:                 s.effective,
		GroupApprovalLock:         s.groupApprovalLock,
		Log:                       s.log,
		NewWorkspaces:             s.newWorkspaces,
		Now:                       s.now,
		Options: lifecycle.Execution{
			Committee:    s.options.Committee,
			Architect:    s.options.Architect,
			Threads:      s.options.Threads,
			PullRequests: s.options.PullRequests,
		},
		Polls:                 &s.polls,
		ProjectFor:            s.projectFor,
		Ratify:                s.ratify,
		RecoverThread:         s.recoverThread,
		RefreshRuntime:        s.refreshRuntime,
		Repository:            s.repository,
		RoleExecution:         s.roleExecution,
		RuntimeOf:             s.lifecycleRuntimeOf,
		ShedMore:              s.shedMore,
		ShedRedraft:           s.shedRedraft,
		Step:                  s.step,
		Stoppable:             s.stoppable,
		Store:                 func() *runtime.Store { return s.store },
		MemoryTurns:           memoryTurns,
		UnresolvedWork:        unresolvedWork,
		LearningsIncorporated: learningsIncorporated,
		ThreadRunner:          s.threadRunner,
	})
}

func (s *Service) lifecycleRuntimeOf(id config.ProjectID) (*config.Config, *lifecycle.Project) {
	cfg, p := s.runtimeOf(id)
	if p == nil {
		return cfg, nil
	}
	return cfg, &lifecycle.Project{ID: p.id, Repository: p.repository}
}

func (s *Service) ensureWorkgroups(ctx context.Context, cfg *config.Config, projects []*activeProject, archives []runtime.Archive) error {
	var ps []*lifecycle.Project
	for _, p := range projects {
		ps = append(ps, &lifecycle.Project{ID: p.id, Repository: p.repository})
	}
	return s.lifecycle().EnsureWorkgroups(ctx, cfg, ps, archives)
}

var resolved = lifecycle.Resolved

type workgroupResolver = lifecycle.WorkgroupResolver

func (s *Service) resolveBase(ctx context.Context, cfg *config.Config, repo *trace.Repository, stream config.WorkstreamID, g workspace.Provider) (seal.Base, error) {
	return s.lifecycle().ResolveBase(ctx, cfg, repo, stream, g)
}

var dependentBaseChanged = lifecycle.DependentBaseChanged

var baseWaiting = lifecycle.BaseWaiting

func (s *Service) baseWaitPass(ctx context.Context, repo *trace.Repository) error {
	return s.lifecycle().BaseWaitPass(ctx, repo)
}

var branchBase = lifecycle.BranchBase

var errDriftAwaits = lifecycle.ErrDriftAwaits

var driftWorkspaces = lifecycle.DriftWorkspaces

var driftResolveTurnID = lifecycle.DriftResolveTurnID

var driftReviewTurnID = lifecycle.DriftReviewTurnID

type resolutions = lifecycle.Resolutions

var driftReviewerSelection = lifecycle.DriftReviewerSelection

var resolvingMove = lifecycle.ResolvingMove

var carriedOnto = lifecycle.CarriedOnto

var carryDrift = lifecycle.CarryDrift

var unitRebaseConflicts = lifecycle.UnitRebaseConflicts

var unitDrift = lifecycle.UnitDrift

var driftDiffTool = lifecycle.DriftDiffTool

func (s *Service) contractGuardRefusal(wt *trace.Repository, g trace.Workgroup, spec trace.ProjectSpec) (string, error) {
	return s.lifecycle().ContractGuardRefusal(wt, g, spec)
}

func (s *Service) projectSpecRefreshPass(ctx context.Context, repository *trace.Repository) error {
	return s.lifecycle().ProjectSpecRefreshPass(ctx, repository)
}

const requestChangeTool = lifecycle.RequestChangeTool

const decideChangeTool = lifecycle.DecideChangeTool

const changeGuidance = lifecycle.ChangeGuidance

type ChangeRequest = lifecycle.ChangeRequest

type ChangeDecision = lifecycle.ChangeDecision

type WorkgroupChange = lifecycle.WorkgroupChange

var workgroupChangeDocument = lifecycle.WorkgroupChangeDocument

var readChanges = lifecycle.ReadChanges

var changeHeld = lifecycle.ChangeHeld

var changeRepositoryHeld = lifecycle.ChangeRepositoryHeld

const deltaTurnAction = lifecycle.DeltaTurnAction

type deltaRunner = lifecycle.DeltaRunner

var decodeDeltaTurn = lifecycle.DecodeDeltaTurn

var deltaTurnID = lifecycle.DeltaTurnID

func (s *Service) changePass(ctx context.Context, repo *trace.Repository) error {
	return s.lifecycle().ChangePass(ctx, repo)
}

func (s *Service) chiefRepository(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (target *trace.Repository, host, bundleStream config.WorkstreamID, close func(), err error) {
	return s.lifecycle().ChiefRepository(ctx, repository, stream)
}

func (s *Service) archive(raw string) (ArchiveResponse, *APIError) {
	return s.lifecycle().Archive(raw)
}

func (s *Service) unarchive(raw string) (ArchiveResponse, *APIError) {
	return s.lifecycle().Unarchive(raw)
}

func (s *Service) archivedWorkstream(raw string) (runtime.Archive, bool) {
	return s.lifecycle().ArchivedWorkstream(raw)
}

var archivedIn = lifecycle.ArchivedIn

type archiveCleanup = lifecycle.ArchiveCleanup

var readMemberEvidence = lifecycle.ReadMemberEvidence

var archiveResolved = lifecycle.ArchiveResolved

func (s *Service) memberArchives() []runtime.Archive {
	return s.lifecycle().MemberArchives()
}

func (s *Service) installMemberLifecycle(repo *trace.Repository) {
	s.lifecycle().InstallMemberLifecycle(repo)
}

func (s *Service) liveWorkgroupLeader(g trace.Workgroup) (config.WorkstreamID, error) {
	return s.lifecycle().LiveWorkgroupLeader(g)
}

func (s *Service) firstLiveWorkgroupMember(g trace.Workgroup) (config.WorkstreamID, error) {
	return s.lifecycle().FirstLiveWorkgroupMember(g)
}

func (s *Service) workgroupFullyRetired(ctx context.Context, g trace.Workgroup) (bool, error) {
	return s.lifecycle().WorkgroupFullyRetired(ctx, g)
}

func (s *Service) recoverArchiveLifecycle(ctx context.Context) {
	s.lifecycle().RecoverArchiveLifecycle(ctx)
}

const ProjectSpecDraftTool = lifecycle.ProjectSpecDraftTool

func (s *Service) DraftWorkgroup(ctx context.Context, members []config.WorkstreamID) (config.WorkgroupID, *APIError) {
	return s.lifecycle().DraftWorkgroup(ctx, members)
}

func (s *Service) projectSpecDraftPass(ctx context.Context, repository *trace.Repository) error {
	return s.lifecycle().ProjectSpecDraftPass(ctx, repository)
}

var leastWorkstream = lifecycle.LeastWorkstream

func (s *Service) crossRepoMemberRepos(ctx context.Context, g trace.Workgroup) ([]trace.CrossRepoMember, error) {
	return s.lifecycle().CrossRepoMemberRepos(ctx, g)
}

var nextReadyTurn = lifecycle.NextReadyTurn

const moveWorkstreamTool = lifecycle.MoveWorkstreamTool

const workstreamMovePrefix = lifecycle.WorkstreamMovePrefix

var reviewMoves = lifecycle.ReviewMoves

var reopenHeld = lifecycle.ReopenHeld

func (s *Service) ownerMoveWorkstream(ctx context.Context, raw string, req WorkstreamMoveRequest) (WorkstreamMoveResponse, *APIError) {
	return s.lifecycle().OwnerMoveWorkstream(ctx, raw, req)
}

var turnDirectories = lifecycle.TurnDirectories

type workspaceCleanup = lifecycle.WorkspaceCleanup

var edgeObservationAt = lifecycle.EdgeObservationAt

var edgeWaiting = lifecycle.EdgeWaiting

var edgeWaitDisplay = lifecycle.EdgeWaitDisplay

var pendingMergeOrderReferences = lifecycle.PendingMergeOrderReferences

var edgePublicationWait = lifecycle.EdgePublicationWait

var unresolvedMergeOrderEdge = lifecycle.UnresolvedMergeOrderEdge

var workgroupMergeOrderResolved = lifecycle.WorkgroupMergeOrderResolved

var workgroupEdgePending = lifecycle.WorkgroupEdgePending

func (s *Service) edgeResolverPass(ctx context.Context, repo *trace.Repository) error {
	return s.lifecycle().EdgeResolverPass(ctx, repo)
}

func (s *Service) edgeWaitPass(ctx context.Context, repo *trace.Repository) error {
	return s.lifecycle().EdgeWaitPass(ctx, repo)
}

const baseRefreshAction = lifecycle.BaseRefreshAction

const deliveryReviewAction = lifecycle.DeliveryReviewAction

const publishUpstreamAction = lifecycle.PublishUpstreamAction

const baseObservationDocument = lifecycle.BaseObservationDocument

type baseObservation = lifecycle.BaseObservation

type baseRefresher = lifecycle.BaseRefresher

var baseObservationAt = lifecycle.BaseObservationAt

var canonicalPublication = lifecycle.CanonicalPublication

var upstreamDeliveryPending = lifecycle.UpstreamDeliveryPending

const baseWaitSubject = lifecycle.BaseWaitSubject

func (s *Service) baseUpstream(ctx context.Context, raw string, req BaseUpstreamRequest) (BaseUpstreamResponse, *APIError) {
	return s.lifecycle().BaseUpstream(ctx, raw, req)
}

const DriftAction = lifecycle.DriftAction

const driftSubject = lifecycle.DriftSubject

const driftPath = lifecycle.DriftPath

const driftDocument = lifecycle.DriftDocument

const driftReplayed = lifecycle.DriftReplayed

const driftRebased = lifecycle.DriftRebased

const driftHeld = lifecycle.DriftHeld

type DriftRebase = lifecycle.DriftRebase

var driftIDs = lifecycle.DriftIDs

var driftNumber = lifecycle.DriftNumber

type drifter struct{ *foreman }

var driftRebaseNotes = lifecycle.DriftRebaseNotes

const handBackDriftTool = lifecycle.HandBackDriftTool

type DriftHandback = lifecycle.DriftHandback

const driftRequestSubject = lifecycle.DriftRequestSubject

func (s *Service) rebaseProject(ctx context.Context, req ProjectRebaseRequest) (ProjectRebaseResponse, *APIError) {
	return s.lifecycle().RebaseProject(ctx, req)
}

func (s *Service) rebaseWorkstream(ctx context.Context, raw string) (WorkstreamRebaseResponse, *APIError) {
	return s.lifecycle().RebaseWorkstream(ctx, raw)
}

var latestDrift = lifecycle.LatestDrift

const edgePoll = lifecycle.EdgePoll

type pullRequestAttempts = lifecycle.PullRequestAttempts

func (c *runtimeControls) lifecycle() *lifecycle.Controls {
	return &lifecycle.Controls{Load: func() *lifecycle.Controller {
		s := c.service.Load()
		if s == nil {
			return nil
		}
		return s.lifecycle()
	}}
}

func (c *runtimeControls) ChangeControl(r *trace.Repository, scope coreadapter.Scope, decide bool) coreadapter.Tool {
	return c.lifecycle().ChangeControl(r, scope, decide)
}

func (c *runtimeControls) MoveWorkstream(r *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return c.lifecycle().MoveWorkstream(r, scope)
}

func (c *runtimeControls) HandBackDrift(r *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	return c.lifecycle().HandBackDrift(r, scope, now)
}

func (f *foreman) driftDriver() *lifecycle.Foreman {
	d := lifecycle.NewForeman(f.s.lifecycle(), f.cfg, f.repository)
	d.NextDrift = f.nextDrift
	return d
}

func (d drifter) driftDriver() lifecycle.Drifter {
	return lifecycle.NewDrifter(d.s.lifecycle(), d.cfg, d.repository)
}

var driftBranch = lifecycle.DriftBranch
