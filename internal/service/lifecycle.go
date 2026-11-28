package service

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
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
		WorkstreamEnded:           s.workstreamEnded,
		Options: lifecycle.Execution{
			Committee:    s.options.Committee,
			Architect:    s.options.Architect,
			Threads:      s.options.Threads,
			PullRequests: s.options.PullRequests,
		},
		Polls:              &s.polls,
		ProjectFor:         s.projectFor,
		Ratify:             s.ratify,
		RecoverThread:      s.recoverThread,
		RefreshRuntime:     s.refreshRuntime,
		Repository:         s.repository,
		RoleExecution:      s.roleExecution,
		RuntimeOf:          s.lifecycleRuntimeOf,
		ShedMore:           s.shedMore,
		ShedRedraft:        s.shedRedraft,
		Step:               s.step,
		Stoppable:          s.stoppable,
		Store:              func() *runtime.Store { return s.store },
		MemoryTurns:        memoryTurns,
		UnresolvedWork:     unresolvedWork,
		KeepForPublication: s.keepForCleanup,
		ThreadRunner:       s.threadRunner,
		Router:             routerPort{s},
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

func (s *Service) resolveBase(ctx context.Context, cfg *config.Config, repo *trace.Repository, stream config.WorkstreamID, g workspace.Provider) (seal.Base, error) {
	return s.lifecycle().ResolveBase(ctx, cfg, repo, stream, g)
}

var dependentBaseChanged = lifecycle.DependentBaseChanged

var baseWaiting = lifecycle.BaseWaiting

var driftWorkspaces = lifecycle.DriftWorkspaces

type resolutions = lifecycle.Resolutions

func (s *Service) contractGuardRefusal(wt *trace.Repository, g trace.Workgroup, spec trace.ProjectSpec) (string, error) {
	return s.lifecycle().ContractGuardRefusal(wt, g, spec)
}

const requestChangeTool = lifecycle.RequestChangeTool

const decideChangeTool = lifecycle.DecideChangeTool

type ChangeDecision = lifecycle.ChangeDecision

var workgroupChangeDocument = lifecycle.WorkgroupChangeDocument

var readChanges = lifecycle.ReadChanges

var changeHeld = lifecycle.ChangeHeld

var changeRepositoryHeld = lifecycle.ChangeRepositoryHeld

const deltaTurnAction = lifecycle.DeltaTurnAction

type deltaRunner = lifecycle.DeltaRunner

func (s *Service) managerRepository(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (target *trace.Repository, host, bundleStream config.WorkstreamID, close func(), err error) {
	return s.lifecycle().ManagerRepository(ctx, repository, stream)
}

func (s *Service) managerRepositoryForRead(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (target *trace.Repository, host, bundleStream config.WorkstreamID, close func(), err error) {
	return s.lifecycle().ManagerRepositoryForRead(ctx, repository, stream)
}

func (s *Service) archive(raw string) (api.ArchiveResponse, *APIError) {
	return s.lifecycle().Archive(raw)
}

func (s *Service) archivedWorkstream(raw string) (runtime.Archive, bool) {
	return s.lifecycle().ArchivedWorkstream(raw)
}

var archivedBy = lifecycle.ArchivedBy

type archiveCleanup = lifecycle.ArchiveCleanup

var readMemberEvidence = lifecycle.ReadMemberEvidence

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

const ProjectSpecDraftTool = lifecycle.ProjectSpecDraftTool

func (s *Service) DraftWorkgroup(ctx context.Context, members []config.WorkstreamID) (config.WorkgroupID, *APIError) {
	return s.lifecycle().DraftWorkgroup(ctx, members)
}

func (s *Service) crossRepoMemberRepos(ctx context.Context, g trace.Workgroup) ([]trace.CrossRepoMember, error) {
	return s.lifecycle().CrossRepoMemberRepos(ctx, g)
}

const moveWorkstreamTool = lifecycle.MoveWorkstreamTool

type workspaceCleanup = lifecycle.WorkspaceCleanup

var edgeWaiting = lifecycle.EdgeWaiting

var workgroupEdgePending = lifecycle.WorkgroupEdgePending

const baseRefreshAction = lifecycle.BaseRefreshAction

const deliveryReviewAction = lifecycle.DeliveryReviewAction

const publishUpstreamAction = lifecycle.PublishUpstreamAction

type baseRefresher = lifecycle.BaseRefresher

var baseObservationAt = lifecycle.BaseObservationAt

const baseWaitSubject = lifecycle.BaseWaitSubject

const DriftAction = lifecycle.DriftAction

const driftSubject = lifecycle.DriftSubject

const driftReplayed = lifecycle.DriftReplayed

const driftRebased = lifecycle.DriftRebased

const driftHeld = lifecycle.DriftHeld

type DriftRebase = lifecycle.DriftRebase

var driftNumber = lifecycle.DriftNumber

type drifter struct{ *foreman }

const handBackDriftTool = lifecycle.HandBackDriftTool

const (
	dropDependencyTool = lifecycle.DropDependencyTool
	retryChangeTool    = lifecycle.RetryChangeTool
)

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
