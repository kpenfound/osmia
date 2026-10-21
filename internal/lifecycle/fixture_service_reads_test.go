package lifecycle_test

import (
	"context"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) activityView() ActivityResponse {
	return service.InspectForTest(s.core()).ActivityView()
}
func (s *Service) conversationTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	return service.InspectForTest(s.core()).ConversationTrace(raw)
}
func (s *Service) conversationList(raw string) (ConversationResponse, *APIError) {
	return service.InspectForTest(s.core()).ConversationList(raw)
}
func (s *Service) deliveryEntry(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	return service.InspectForTest(s.core()).DeliveryEntry(ctx, repository, stream)
}
func (s *Service) groupApprovalLock(id config.WorkgroupID) *sync.Mutex {
	return service.InspectForTest(s.core()).GroupApprovalLock(id)
}
func (s *Service) approvalTrace(ctx context.Context, repo *trace.Repository, id config.WorkgroupID) (*trace.Repository, trace.Workgroup, error) {
	return service.InspectForTest(s.core()).ApprovalTrace(ctx, repo, id)
}
func (s *Service) prepareGroupApproval(ctx context.Context, decision trace.GroupApproval) error {
	return service.InspectForTest(s.core()).PrepareGroupApproval(ctx, decision)
}
func (s *Service) approvalMemberRepository(ctx context.Context, project config.ProjectID) (*trace.Repository, error) {
	return service.InspectForTest(s.core()).ApprovalMemberRepository(ctx, project)
}
func (s *Service) recoverMemberApprovals(ctx context.Context, wt *trace.Repository, g trace.Workgroup) (trace.GroupApproval, bool, error) {
	return service.InspectForTest(s.core()).RecoverMemberApprovals(ctx, wt, g)
}
func (s *Service) configuration() ConfigResponse {
	return service.InspectForTest(s.core()).Configuration()
}
func (s *Service) crossRepoWorkgroupIDs(ctx context.Context, repository *trace.Repository) ([]config.WorkgroupID, error) {
	return service.InspectForTest(s.core()).CrossRepoWorkgroupIDs(ctx, repository)
}
func (s *Service) workgroupInbox(ctx context.Context, known *trace.Repository, id config.WorkgroupID) ([]InboxEntry, *APIError) {
	return service.InspectForTest(s.core()).WorkgroupInbox(ctx, known, id)
}
func (s *Service) openDecisions(ctx context.Context, repository *trace.Repository, w trace.WorkstreamStatus) ([]InboxEntry, error) {
	return service.InspectForTest(s.core()).OpenDecisions(ctx, repository, w)
}
func (s *Service) ratifyMember(ctx context.Context, o *shedOwner, asked shed.Pin) (RatifyResponse, *APIError) {
	return service.InspectForTest(s.core()).RatifyMember(ctx, o, asked)
}
func (s *Service) shedAction(raw string, states ...string) (*shedOwner, *APIError) {
	return service.InspectForTest(s.core()).ShedAction(raw, states...)
}
func (s *Service) presentWorkstream(repo *trace.Repository, w *WorkstreamStatus) error {
	return service.InspectForTest(s.core()).PresentWorkstream(repo, w)
}
func (s *Service) reload() (ReloadResponse, *APIError) {
	return service.InspectForTest(s.core()).Reload()
}

func (s *Service) halt(active *activeProject) error {
	return service.InspectForTest(s.core()).Halt(active.id)
}
func (s *Service) statuses() ([]WorkstreamStatus, map[config.WorkstreamID]Diagnostic, *APIError) {
	return service.InspectForTest(s.core()).Statuses()
}
func (s *Service) statusList() StatusResponse { return service.InspectForTest(s.core()).StatusList() }
func (s *Service) workstreamStatus(raw string) (WorkstreamStatus, *APIError) {
	return service.InspectForTest(s.core()).WorkstreamStatus(raw)
}
