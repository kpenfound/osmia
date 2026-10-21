package lifecycle_test

import (
	"context"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

var migrationActor = lifecycle.MigrationActor

func (s *Service) ensureWorkgroups(ctx context.Context, cfg *config.Config, projects []*activeProject, archives []runtime.Archive) error {
	var ps []*lifecycle.Project
	for _, p := range projects {
		ps = append(ps, &lifecycle.Project{ID: p.id, Repository: p.repository})
	}
	return s.lifecycle().EnsureWorkgroups(ctx, cfg, ps, archives)
}

var resolved = lifecycle.Resolved

var dependentBaseChanged = lifecycle.DependentBaseChanged
var baseWaiting = lifecycle.BaseWaiting

func (s *Service) baseWaitPass(ctx context.Context, repo *trace.Repository) error {
	return s.lifecycle().BaseWaitPass(ctx, repo)
}

var branchBase = lifecycle.BranchBase
var errDriftAwaits = lifecycle.ErrDriftAwaits

var driftWorkspaces = lifecycle.DriftWorkspaces
var driftBranch = lifecycle.DriftBranch
var driftResolveTurnID = lifecycle.DriftResolveTurnID
var driftFixTurnID = lifecycle.DriftFixTurnID
var driftReviewTurnID = lifecycle.DriftReviewTurnID
var unitRebaseConflicts = lifecycle.UnitRebaseConflicts
var unitDrift = lifecycle.UnitDrift

const driftScopeRule = lifecycle.DriftScopeRule

var driftMasonSystemPrompt = lifecycle.DriftMasonSystemPrompt
var driftReviewerSystemPrompt = lifecycle.DriftReviewerSystemPrompt
var driftDiffTool = lifecycle.DriftDiffTool
var readProjectSpecDraftInput = lifecycle.ReadProjectSpecDraftInput

func (s *Service) projectSpecRefreshPass(ctx context.Context, repository *trace.Repository) error {
	return s.lifecycle().ProjectSpecRefreshPass(ctx, repository)
}

const requestChangeTool = lifecycle.RequestChangeTool
const decideChangeTool = lifecycle.DecideChangeTool

type ChangeRequest = lifecycle.ChangeRequest
type ChangeDecision = lifecycle.ChangeDecision
type WorkgroupChange = lifecycle.WorkgroupChange

var readChanges = lifecycle.ReadChanges

func (s *Service) saveChange(ctx context.Context, repo *trace.Repository, c *WorkgroupChange) error {
	return s.lifecycle().SaveChange(ctx, repo, c)
}
func (s *Service) changeContext(ctx context.Context, raw string) (*trace.Repository, trace.Workgroup, config.WorkstreamID, func(), error) {
	return s.lifecycle().ChangeContext(ctx, raw)
}
func (s *Service) changeApplicationLock(group config.WorkgroupID) *sync.RWMutex {
	return s.lifecycle().ChangeApplicationLock(group)
}
func (s *Service) requestChange(ctx context.Context, raw string, req ChangeRequest) (WorkgroupChange, *APIError) {
	return s.lifecycle().RequestChange(ctx, raw, req)
}
func (s *Service) decideChange(ctx context.Context, raw, id string, d ChangeDecision) (WorkgroupChange, *APIError) {
	return s.lifecycle().DecideChange(ctx, raw, id, d)
}

var changeHeld = lifecycle.ChangeHeld

func (s *Service) changeBoundary(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, c WorkgroupChange, release bool) error {
	return s.lifecycle().ChangeBoundary(ctx, repo, stream, c, release)
}

var changeIdle = lifecycle.ChangeIdle
var changeEffectStarted = lifecycle.ChangeEffectStarted

const deltaDraftTool = lifecycle.DeltaDraftTool
const deltaReviewTool = lifecycle.DeltaReviewTool
const deltaReplyTool = lifecycle.DeltaReplyTool
const deltaTurnAction = lifecycle.DeltaTurnAction

type deltaTurnInput = lifecycle.DeltaTurnInput

var decodeDeltaTurn = lifecycle.DecodeDeltaTurn
var deltaTurnID = lifecycle.DeltaTurnID

func (s *Service) changePass(ctx context.Context, repo *trace.Repository) error {
	return s.lifecycle().ChangePass(ctx, repo)
}
func (s *Service) advanceChanges(ctx context.Context, wt *trace.Repository, g trace.Workgroup) error {
	return s.lifecycle().AdvanceChanges(ctx, wt, g)
}
func (s *Service) deltaTool(wt *trace.Repository, c WorkgroupChange, agent, name string) coreadapter.Tool {
	return s.lifecycle().DeltaTool(wt, c, agent, name)
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

var archiveResolved = lifecycle.ArchiveResolved

func (s *Service) liveWorkgroupLeader(g trace.Workgroup) (config.WorkstreamID, error) {
	return s.lifecycle().LiveWorkgroupLeader(g)
}
func (s *Service) workgroupFullyRetired(ctx context.Context, g trace.Workgroup) (bool, error) {
	return s.lifecycle().WorkgroupFullyRetired(ctx, g)
}

const ProjectSpecDraftTool = lifecycle.ProjectSpecDraftTool
const projectSpecDraftSubject = lifecycle.ProjectSpecDraftSubject

var projectSpecDraftActor = lifecycle.ProjectSpecDraftActor

type projectSpecDraft = lifecycle.ProjectSpecDraft
type projectSpecDraftSlice = lifecycle.ProjectSpecDraftSlice

func (s *Service) DraftWorkgroup(ctx context.Context, members []config.WorkstreamID) (config.WorkgroupID, *APIError) {
	return s.lifecycle().DraftWorkgroup(ctx, members)
}
func (s *Service) draftProjectSpecIfReady(ctx context.Context, known *trace.Repository, id config.WorkgroupID) error {
	return s.lifecycle().DraftProjectSpecIfReady(ctx, known, id)
}
func (s *Service) projectSpecTurnDirectoryFor(host config.WorkstreamID) string {
	return s.lifecycle().ProjectSpecTurnDirectoryFor(host)
}
func (s *Service) enqueueProjectSpecDraft(ctx context.Context, wt *trace.Repository, g trace.Workgroup, host config.WorkstreamID, members []trace.CrossRepoMember, turn string) error {
	return s.lifecycle().EnqueueProjectSpecDraft(ctx, wt, g, host, members, turn)
}

var memberSpecAndPlan = lifecycle.MemberSpecAndPlan

func (s *Service) projectSpecDraftPass(ctx context.Context, repository *trace.Repository) error {
	return s.lifecycle().ProjectSpecDraftPass(ctx, repository)
}

var leastWorkstream = lifecycle.LeastWorkstream

func (s *Service) crossRepoMemberRepos(ctx context.Context, g trace.Workgroup) ([]trace.CrossRepoMember, error) {
	return s.lifecycle().CrossRepoMemberRepos(ctx, g)
}

var nextReadyTurn = lifecycle.NextReadyTurn
var workstreamMoveDocument = lifecycle.WorkstreamMoveDocument

func (s *Service) ownerMoveWorkstream(ctx context.Context, raw string, req WorkstreamMoveRequest) (WorkstreamMoveResponse, *APIError) {
	return s.lifecycle().OwnerMoveWorkstream(ctx, raw, req)
}

const reviewInputsDirectory = lifecycle.ReviewInputsDirectory

type cleanupFailure = lifecycle.CleanupFailure

const edgeObservationDocument = lifecycle.EdgeObservationDocument
const edgeObservationPath = lifecycle.EdgeObservationPath

type edgeMergeStatus = lifecycle.EdgeMergeStatus
type edgeObservation = lifecycle.EdgeObservation

var edgeObservationAt = lifecycle.EdgeObservationAt
var edgeWaitMessage = lifecycle.EdgeWaitMessage
var edgeWaiting = lifecycle.EdgeWaiting
var edgeWaitDisplay = lifecycle.EdgeWaitDisplay
var unresolvedMergeOrderEdge = lifecycle.UnresolvedMergeOrderEdge
var workgroupMergeOrderResolved = lifecycle.WorkgroupMergeOrderResolved
var workgroupEdgePending = lifecycle.WorkgroupEdgePending

func (s *Service) upstreamMergeStatus(ctx context.Context, repo *trace.Repository, edge trace.Edge) (edgeMergeStatus, error) {
	return s.lifecycle().UpstreamMergeStatus(ctx, repo, edge)
}
func (s *Service) edgeResolverPass(ctx context.Context, repo *trace.Repository) error {
	return s.lifecycle().EdgeResolverPass(ctx, repo)
}
func (s *Service) edgeWaitPass(ctx context.Context, repo *trace.Repository) error {
	return s.lifecycle().EdgeWaitPass(ctx, repo)
}

const baseRefreshAction = lifecycle.BaseRefreshAction
const deliveryReviewAction = lifecycle.DeliveryReviewAction
const publishUpstreamAction = lifecycle.PublishUpstreamAction

type baseRefreshInput = lifecycle.BaseRefreshInput

var baseObservationAt = lifecycle.BaseObservationAt
var upstreamDeliveryPending = lifecycle.UpstreamDeliveryPending

const baseWaitSubject = lifecycle.BaseWaitSubject

func (s *Service) moveOntoUpstream(ctx context.Context, cfg *config.Config, repository *trace.Repository, g workspace.Provider, stream, base config.WorkstreamID) (BaseUpstreamResponse, *APIError) {
	return s.lifecycle().MoveOntoUpstream(ctx, cfg, repository, g, stream, base)
}

const DriftAction = lifecycle.DriftAction
const driftSubject = lifecycle.DriftSubject
const driftDocument = lifecycle.DriftDocument
const driftReplayed = lifecycle.DriftReplayed
const driftCarrying = lifecycle.DriftCarrying
const driftRebased = lifecycle.DriftRebased
const driftConflicted = lifecycle.DriftConflicted
const driftHeld = lifecycle.DriftHeld

type DriftRebase = lifecycle.DriftRebase

var driftIDs = lifecycle.DriftIDs

func (s *Service) handBackDrift(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, note string, actor trace.Actor, turn string, at time.Time) (int, *APIError) {
	return s.lifecycle().HandBackDrift(ctx, repo, stream, note, actor, turn, at)
}

var chiefHandbacks = lifecycle.ChiefHandbacks

const driftRequestSubject = lifecycle.DriftRequestSubject

func (s *Service) rebaseProject(ctx context.Context, req ProjectRebaseRequest) (ProjectRebaseResponse, *APIError) {
	return s.lifecycle().RebaseProject(ctx, req)
}
func (s *Service) rebaseWorkstream(ctx context.Context, raw string) (WorkstreamRebaseResponse, *APIError) {
	return s.lifecycle().RebaseWorkstream(ctx, raw)
}

var latestDrift = lifecycle.LatestDrift

const edgePoll = lifecycle.EdgePoll

func (s *Service) applyChange(ctx context.Context, wt *trace.Repository, g trace.Workgroup, c *WorkgroupChange) error {
	return s.lifecycle().ApplyChange(ctx, wt, g, c)
}
func (s *Service) changeSealReady(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, id string) (bool, error) {
	return s.lifecycle().ChangeSealReady(ctx, repo, stream, id)
}
func (s *Service) cancelChangeMember(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, c WorkgroupChange) error {
	return s.lifecycle().CancelChangeMember(ctx, repo, stream, c)
}
func (s *Service) settleChangeRequester(ctx context.Context, c WorkgroupChange) error {
	return s.lifecycle().SettleChangeRequester(ctx, c)
}
func (s *Service) reworkChangeMember(ctx context.Context, repo *trace.Repository, row amendment.StreamImpact, c *WorkgroupChange) error {
	return s.lifecycle().ReworkChangeMember(ctx, repo, row, c)
}
func (s *Service) planChangeMember(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, sl trace.Slice, id string) error {
	return s.lifecycle().PlanChangeMember(ctx, repo, stream, sl, id)
}
func (c *runtimeControls) moveWorkstream(r *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return c.lifecycle().MoveWorkstream(r, scope)
}
func (c *runtimeControls) handBackDrift(r *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	return c.lifecycle().HandBackDrift(r, scope, now)
}

type archiveCleanup struct {
	s          *Service
	workspaces *workspaceCleanup

	finish func(config.Root, config.ProjectID, config.WorkstreamID) error
	_core  *lifecycle.ArchiveCleanup
}

func (c *archiveCleanup) Pass(ctx context.Context) error {
	return (c.coreCleanup()).Pass(ctx)
}

func (c *archiveCleanup) prepare(ctx context.Context, a runtime.Archive) (eligible bool, reason string, err error) {
	return (c.coreCleanup()).Prepare(ctx, a)
}

type baseRefresher struct {
	s          *Service
	repository *trace.Repository
}

func (b *baseRefresher) Pass(ctx context.Context) error {
	return (&lifecycle.BaseRefresher{S: b.s.lifecycle(), Repository: b.repository}).Pass(ctx)
}

func (b *baseRefresher) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return (&lifecycle.BaseRefresher{S: b.s.lifecycle(), Repository: b.repository}).Inspect(ctx, op)
}
func (b *baseRefresher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return (&lifecycle.BaseRefresher{S: b.s.lifecycle(), Repository: b.repository}).Apply(ctx, op)
}

type workspaceCleanup struct {
	cfg        *config.Config
	repository *trace.Repository
	now        func() time.Time
	failures   map[string]cleanupFailure
	_core      *lifecycle.WorkspaceCleanup
}

func (c *workspaceCleanup) Pass(ctx context.Context) error {
	return (c.coreCleanup()).Pass(ctx)
}

func (c *workspaceCleanup) busy(stream config.WorkstreamID) (map[string]bool, bool, error) {
	return (c.coreCleanup()).Busy(stream)
}

func (f *foreman) requestDrift(ctx context.Context, stream config.WorkstreamID, why string) error {
	return (f.driftDriver()).RequestDrift(ctx, stream, why)
}

type drifter struct{ *foreman }

func (d drifter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return (lifecycle.NewDrifter(d.s.lifecycle(), d.cfg, d.repository)).Inspect(ctx, op)
}
func (d drifter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return (lifecycle.NewDrifter(d.s.lifecycle(), d.cfg, d.repository)).Apply(ctx, op)
}

type resolutions struct{ streamWorkspaces }

func (r resolutions) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	return (lifecycle.Resolutions{StreamWorkspaces: r.streamWorkspaces}).Acquire(ctx, req)
}
func (r resolutions) selection(ctx context.Context, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	return (lifecycle.Resolutions{StreamWorkspaces: r.streamWorkspaces}).Selection(ctx, scope, execution)
}
func (r resolutions) capture(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView) error {
	return (lifecycle.Resolutions{StreamWorkspaces: r.streamWorkspaces}).Capture(ctx, scope, view)
}

func (d drifter) resolvePrompt(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (string, error) {
	return (lifecycle.NewDrifter(d.s.lifecycle(), d.cfg, d.repository)).ResolvePrompt(ctx, stream, rebase)
}
func (d drifter) fixPrompt(stream config.WorkstreamID, rebase DriftRebase) (string, error) {
	return (lifecycle.NewDrifter(d.s.lifecycle(), d.cfg, d.repository)).FixPrompt(stream, rebase)
}
func (d drifter) reviewPrompt(ctx context.Context, stream config.WorkstreamID, rebase DriftRebase) (string, error) {
	return (lifecycle.NewDrifter(d.s.lifecycle(), d.cfg, d.repository)).ReviewPrompt(ctx, stream, rebase)
}

func (f *foreman) driftDue(stream config.WorkstreamID, now time.Time) (string, error) {
	return (f.driftDriver()).DriftDue(stream, now)
}
func (f *foreman) drifts(ctx context.Context) error {
	driver := f.driftDriver()
	err := driver.Drifts(ctx)
	f.nextDrift = driver.NextDrift
	return err
}
func (f *foreman) askDrift(ctx context.Context, stream config.WorkstreamID, at time.Time) (int, error) {
	return (f.driftDriver()).AskDrift(ctx, stream, at)
}

type deltaRunner struct{ s *Service }

func (d deltaRunner) load(ctx context.Context, in deltaTurnInput) (*trace.Repository, WorkgroupChange, bool, func(), error) {
	return (lifecycle.DeltaRunner{S: d.s.lifecycle()}).Load(ctx, in)
}
func (d deltaRunner) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return (lifecycle.DeltaRunner{S: d.s.lifecycle()}).Inspect(ctx, op)
}
func (d deltaRunner) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return (lifecycle.DeltaRunner{S: d.s.lifecycle()}).Apply(ctx, op)
}

type workgroupResolver struct {
	s          *Service
	repository *trace.Repository
}

func (w *workgroupResolver) Pass(ctx context.Context) error {
	return (&lifecycle.WorkgroupResolver{S: w.s.lifecycle(), Repository: w.repository}).Pass(ctx)
}
func (s *Service) lifecycle() *lifecycle.Controller {
	c := service.InspectForTest(s.core()).LifecycleController()
	c.Options.Architect = s.options.Architect
	c.Options.Committee = s.options.Committee
	c.Options.PullRequests = s.options.PullRequests
	c.Execution = s.execution
	c.Intake = s.intake
	if s.options.Reconciliation.Now != nil {
		c.Now = s.options.Reconciliation.Now
	}
	if s.boundary != nil {
		c.Step = s.boundary
	}
	return c
}
func (c *runtimeControls) lifecycle() *lifecycle.Controls {
	return &lifecycle.Controls{Load: func() *lifecycle.Controller {
		if c == nil || c.service.Load() == nil {
			return nil
		}
		return c.service.Load().lifecycle()
	}}
}
func (f *foreman) driftDriver() *lifecycle.Foreman {
	d := lifecycle.NewForeman(f.s.lifecycle(), f.cfg, f.repository)
	d.NextDrift = f.nextDrift
	return d
}
func (c *workspaceCleanup) coreCleanup() *lifecycle.WorkspaceCleanup {
	if c._core == nil {
		c._core = &lifecycle.WorkspaceCleanup{Cfg: c.cfg, Repository: c.repository, Now: c.now}
	}
	c._core.Cfg = c.cfg
	c._core.Repository = c.repository
	c._core.Now = c.now
	if c._core.Failures == nil {
		c._core.Failures = map[string]lifecycle.CleanupFailure{}
	}
	c.failures = c._core.Failures
	return c._core
}
func (c *archiveCleanup) coreCleanup() *lifecycle.ArchiveCleanup {
	if c._core == nil {
		c._core = &lifecycle.ArchiveCleanup{}
	}
	c._core.S = c.s.lifecycle()
	c._core.Workspaces = c.workspaces.coreCleanup()
	c._core.Finish = c.finish
	return c._core
}
