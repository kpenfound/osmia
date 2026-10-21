package executionjourney

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

type drifter struct{ *foreman }

func (d drifter) driver() *service.TestDriftDriver {
	return service.DriftForTest(d.s.core(), d.repository)
}
func (d drifter) requestDrifts(ctx context.Context, due func(config.WorkstreamID) (string, error)) ([]config.WorkstreamID, error) {
	return d.driver().Request(ctx, due)
}
func (d drifter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.driver().Inspect(ctx, op)
}
func (d drifter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.driver().Apply(ctx, op)
}

type DriftRebase = service.DriftRebase
type Librarian = service.Librarian
type refreshInput = service.TestRefreshInput
type KnowledgeSource = service.KnowledgeSource

const RefreshAction = service.RefreshAction
const DriftAction = service.DriftAction

var stageChiefDocuments = service.TestStageChiefDocuments
var withActions = service.TestWithActions
var driftResolveTurnID = service.TestDriftResolveTurnID
var driftReviewTurnID = service.TestDriftReviewTurnID
var driftDiffTool = service.TestDriftDiffTool
var driftWorkspaces = service.TestDriftWorkspaces
var operationTrailer = service.TestOperationTrailer
var errDriftAwaits = service.TestErrDriftAwaits

const driftRebased = service.TestDriftRebased
const commitChange = service.TestCommitChange
const driftDocument = service.TestDriftDocument
const driftMasonAgent = service.TestDriftMasonAgent
const driftReviewerAgent = service.TestDriftReviewerAgent
const driftsDirectory = service.TestDriftsDirectory
const librarianAgent = service.TestLibrarianAgent

var factoryTarget = runtime.Target{Scope: "factory"}

func (s *Service) admit(cfg *config.Config, r *trace.Repository) func(context.Context, scheduler.Candidate) (bool, error) {
	return service.InspectForTest(s.core()).Admit(cfg, r)
}
func (s *Service) traceView(ctx context.Context, w, kind, sel string) (any, *APIError) {
	return service.InspectForTest(s.core()).TraceView(ctx, w, kind, sel)
}
func (s *Service) capacityStatus(list []WorkstreamStatus) *CapacityStatus {
	return service.InspectForTest(s.core()).Capacity(list)
}
func (s *Service) contestedEntry(r *trace.Repository, w config.WorkstreamID, u string) (InboxEntry, bool, error) {
	return service.InspectForTest(s.core()).ContestedEntry(r, w, u)
}

var contestOptions = service.TestContestOptions
