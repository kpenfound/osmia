package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/eventstream"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func (a TestAccess) ActivityView() ActivityResponse { return a.s.activityView() }
func (a TestAccess) ConversationTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	return a.s.conversationTrace(raw)
}
func (a TestAccess) ConversationList(raw string) (ConversationResponse, *APIError) {
	return a.s.conversationList(raw)
}
func (a TestAccess) DeliveryEntry(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	return a.s.deliveryEntry(ctx, repository, stream)
}
func (a TestAccess) GroupApprovalLock(id config.WorkgroupID) *sync.Mutex {
	return a.s.groupApprovalLock(id)
}
func (a TestAccess) ApprovalTrace(ctx context.Context, repo *trace.Repository, id config.WorkgroupID) (*trace.Repository, trace.Workgroup, error) {
	return a.s.approvalTrace(ctx, repo, id)
}
func (a TestAccess) PrepareGroupApproval(ctx context.Context, decision trace.GroupApproval) error {
	return a.s.prepareGroupApproval(ctx, decision)
}
func (a TestAccess) ApprovalMemberRepository(ctx context.Context, project config.ProjectID) (*trace.Repository, error) {
	return a.s.approvalMemberRepository(ctx, project)
}
func (a TestAccess) RecoverMemberApprovals(ctx context.Context, wt *trace.Repository, g trace.Workgroup) (trace.GroupApproval, bool, error) {
	return a.s.recoverMemberApprovals(ctx, wt, g)
}
func (a TestAccess) Configuration() ConfigResponse { return a.s.configuration() }
func (a TestAccess) CrossRepoWorkgroupIDs(ctx context.Context, repository *trace.Repository) ([]config.WorkgroupID, error) {
	return a.s.crossRepoWorkgroupIDs(ctx, repository)
}
func (a TestAccess) WorkgroupInbox(ctx context.Context, known *trace.Repository, id config.WorkgroupID) ([]InboxEntry, *APIError) {
	return a.s.workgroupInbox(ctx, known, id)
}
func (a TestAccess) OpenDecisions(ctx context.Context, repository *trace.Repository, w trace.WorkstreamStatus) ([]InboxEntry, error) {
	return a.s.openDecisions(ctx, repository, w)
}
func (a TestAccess) RatifyMember(ctx context.Context, o *shedOwner, asked shed.Pin) (RatifyResponse, *APIError) {
	return a.s.ratifyMember(ctx, o, asked)
}
func (a TestAccess) ShedAction(raw string, states ...string) (*shedOwner, *APIError) {
	return a.s.shedAction(raw, states...)
}
func (a TestAccess) PresentWorkstream(repo *trace.Repository, w *WorkstreamStatus) error {
	return a.s.presentWorkstream(repo, w)
}
func (a TestAccess) Reload() (ReloadResponse, *APIError) { return a.s.reload() }

func (a TestAccess) StatusList() StatusResponse { return a.s.statusList() }
func (a TestAccess) WorkstreamStatus(raw string) (WorkstreamStatus, *APIError) {
	return a.s.workstreamStatus(raw)
}

// TestProject identifies a runtime without transferring ownership of its trace.
type TestProject struct {
	ID         config.ProjectID
	Repository *trace.Repository
}

func (a TestAccess) Projects() []TestProject {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	var out []TestProject
	for _, p := range a.s.projects {
		out = append(out, TestProject{p.id, p.repository})
	}
	return out
}

// SetFixtureState updates deliberately stopped runtime bindings in white-box fixtures.
func (a TestAccess) SetFixtureState(cfg *config.Config, opts Options, store *runtime.Store, ps []TestProject) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.s.cfg = cfg
	a.s.options = opts
	a.s.store = store
	var projects []*activeProject
	for _, p := range ps {
		var found *activeProject
		for _, old := range a.s.projects {
			if old.id == p.ID {
				found = old
			}
		}
		if found == nil {
			found = &activeProject{id: p.ID}
		}
		found.repository = p.Repository
		projects = append(projects, found)
	}
	a.s.projects = projects
}
func (a TestAccess) SetActivity(r *activity.Registry) { a.s.activity = r }
func (a TestAccess) SetHub(h *eventstream.Hub)        { a.s.hub = h }
func (a TestAccess) NotifyPass(ctx context.Context, retry time.Duration) {
	newNotifier(a.s, retry, nil).pass(ctx)
}

var TestBaseEntry = baseEntry
var TestReapSandboxes = reapSandboxes
var TestDriftMasonTools = driftMasonTools
var TestMasonGrant = masonGrant
var TestReviewerGrant = reviewerGrant
var TestChiefContext = chiefContext

func RefreshKeyForTest(in refreshInput) string { return in.key() }

type TestNotifyLedger = notifyLedger

const TestNotificationPending = notificationPending

func (a TestAccess) Statuses() ([]WorkstreamStatus, map[config.WorkstreamID]Diagnostic, *APIError) {
	return a.s.statuses()
}
func ThreadWorkspaceAcquireForTest(ctx context.Context, cfg *config.Config, r *trace.Repository, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	return (threadWorkspaces{units: newUnitWorkspaces(cfg, r), drifts: resolutions{StreamWorkspaces: driftWorkspaces(cfg, r)}}).Acquire(ctx, req)
}
func (a TestAccess) SetFixtureStore(store *runtime.Store) { a.s.store = store }
func (a TestAccess) SetFixtureLogger(logger *slog.Logger) { a.s.logger = logger }
