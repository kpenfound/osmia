package executionjourney

import (
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
	"testing"
	"time"
)

type masons struct {
	s          *Service
	cfg        *config.Config
	repository *trace.Repository
}

func (m *masons) driver() *execution.Masons {
	var c *execution.Controller
	if m.s != nil {
		c = m.s.execution()
	}
	return execution.NewMasons(c, m.cfg, m.repository)
}

type foreman struct {
	*masons
}

func (f *foreman) driver() *execution.Foreman { return execution.NewForeman(f.masons.driver()) }

type reviewers struct{ *masons }

func (r *reviewers) driver() *execution.Reviewers { return execution.NewReviewers(r.masons.driver()) }

type checkers struct{ *masons }

func (c *checkers) driver() *execution.Checkers { return execution.NewCheckers(c.masons.driver()) }

type rebaser struct{ *foreman }

func (r rebaser) driver() execution.Rebaser { return execution.NewRebaser(r.foreman.driver()) }

type unitWorkspaces struct {
	execution.UnitWorkspaces
	streamWorkspaces intake.StreamWorkspaces
}

func newUnitWorkspaces(cfg *config.Config, repo *trace.Repository) unitWorkspaces {
	return unitWorkspaces{execution.NewUnitWorkspaces(cfg, repo), intake.StreamWorkspaces{Cfg: cfg, Repository: repo, Directory: intake.UnitsDirectory}}
}
func (u unitWorkspaces) driver() execution.UnitWorkspaces { return u.UnitWorkspaces }
func (m *masons) Pass(ctx context.Context) error          { return m.driver().Pass(ctx) }

func (m *masons) follow(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (string, error) {
	return m.driver().Follow(ctx, stream, unit, state)
}
func (m *masons) read(stream config.WorkstreamID) (execution.Building, bool, error) {
	return m.driver().Read(stream)
}

func (m *masons) start(ctx context.Context, b execution.Building, unit string) (started, blocked bool, err error) {
	return m.driver().Start(ctx, b, unit)
}
func (m *masons) resume(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	return m.driver().Resume(ctx, stream, unit)
}

func (u unitWorkspaces) open(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, error) {
	return u.driver().Open(ctx, stream, unit)
}

func (u unitWorkspaces) find(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, bool, error) {
	return u.driver().Find(ctx, stream, unit)
}
func (u unitWorkspaces) behind(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	return u.driver().Behind(ctx, stream, unit)
}

func (u unitWorkspaces) paths(w workspace.Worktree) ([]string, error) { return u.driver().Paths(w) }
func (u unitWorkspaces) selection(ctx context.Context, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	return u.driver().Selection(ctx, scope, execution)
}

func (c *checkers) Pass(ctx context.Context) error { return c.driver().Pass(ctx) }

func (c *checkers) fail(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, run execution.UnitCheckRun) error {
	return c.driver().Fail(ctx, stream, unit, state, run)
}

func (c *checkers) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return c.driver().Inspect(ctx, op)
}
func (c *checkers) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return c.driver().Apply(ctx, op)
}

func (c *checkers) record(ctx context.Context, in execution.CheckInput, run execution.UnitCheckRun, captured string) (coreadapter.OperationResult, error) {
	return c.driver().Record(ctx, in, run, captured)
}

func (r *reviewers) Pass(ctx context.Context) error { return r.driver().Pass(ctx) }
func (r *reviewers) followQuestion(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState) (string, error) {
	return r.driver().FollowQuestion(ctx, stream, unit, state)
}
func (r *reviewers) one(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, paused bool) error {
	return r.driver().One(ctx, stream, unit, state, paused)
}

func (r *reviewers) storedResult(stream config.WorkstreamID, unit string, state trace.WorkflowState) (service.UnitReviewResult, bool, error) {
	return r.driver().StoredResult(stream, unit, state)
}

func (r *reviewers) applyReview(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, result service.UnitReviewResult) error {
	return r.driver().ApplyReview(ctx, stream, unit, state, result)
}

func (r *reviewers) refreshReview(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, reason string) error {
	return r.driver().RefreshReview(ctx, stream, unit, state, reason)
}

func (r *reviewers) resumeContested(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, contests ...trace.Transition) error {
	return r.driver().ResumeContested(ctx, stream, unit, state, contests...)
}
func (r *reviewers) applyBounceRuling(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, result service.UnitReviewResult, ruling api.ContestedRuling, docs []trace.Document) error {
	return r.driver().ApplyBounceRuling(ctx, stream, unit, state, result, ruling, docs)
}
func (m *masons) resumeBounceRevision(ctx context.Context, stream config.WorkstreamID, unit string, t trace.Transition) error {
	return m.driver().ResumeBounceRevision(ctx, stream, unit, t)
}
func (f *foreman) Pass(ctx context.Context) error { return f.driver().Pass(ctx) }
func (f *foreman) approval(stream config.WorkstreamID, unit string, revision int) (trace.Document, service.UnitReviewResult, bool, error) {
	return f.driver().Approval(stream, unit, revision)
}

func (f *foreman) request(ctx context.Context, stream config.WorkstreamID, unit string, review trace.Document, result service.UnitReviewResult) error {
	return f.driver().Request(ctx, stream, unit, review, result)
}

func (f *foreman) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return f.driver().Inspect(ctx, op)
}
func (f *foreman) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return f.driver().Apply(ctx, op)
}

func (f *foreman) current(ctx context.Context, stream config.WorkstreamID, in execution.LandInput, result service.UnitReviewResult) (string, error) {
	return f.driver().Current(ctx, stream, in, result)
}

func (f *foreman) writing(stream config.WorkstreamID, unit string, dispatched map[string]bool) (bool, error) {
	return f.driver().Writing(stream, unit, dispatched)
}
func (f *foreman) requestRebase(ctx context.Context, stream config.WorkstreamID, unit, onto string) error {
	return f.driver().RequestRebase(ctx, stream, unit, onto)
}

func (m *masons) conflicts(stream config.WorkstreamID, unit string) ([]string, api.UnitRebase, error) {
	return m.driver().Conflicts(stream, unit)
}

func (r rebaser) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return r.driver().Inspect(ctx, op)
}
func (r rebaser) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return r.driver().Apply(ctx, op)
}

func (m *masons) finish(ctx context.Context, b execution.Building, unit string) (moved, blocked bool, err error) {
	return m.driver().Finish(ctx, b, unit)
}
func (m *masons) prepareUnitReview(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, service.UnitReviewIdentity, error) {
	return m.driver().PrepareUnitReview(ctx, stream, unit)
}

func (m *masons) candidateEvidence(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, service.UnitReviewIdentity, error) {
	return m.driver().CandidateEvidence(ctx, stream, unit)
}

func (f *foreman) message(w config.WorkstreamID, in execution.LandInput, review trace.Document, result service.UnitReviewResult, op string) (string, error) {
	return f.driver().Message(w, in, review, result, op)
}
func (f *foreman) requestedAt(w config.WorkstreamID, op string) (time.Time, error) {
	return f.driver().RequestedAt(w, op)
}

type finalReviewer struct {
	s          *Service
	repository *trace.Repository
}

func (d *finalReviewer) driver() *delivery.FinalReviewer {
	var c *delivery.Controller
	if d.s != nil {
		c = d.s.delivery()
	}
	return &delivery.FinalReviewer{S: c, Repository: d.repository}
}

func (d *finalReviewer) Pass(ctx context.Context) error { return d.driver().Pass(ctx) }

func (d *finalReviewer) governing(ctx context.Context, stream config.WorkstreamID) (delivery.FinalReviewInput, seal.Seal, error) {
	return d.driver().Governing(ctx, stream)
}

func (d *finalReviewer) request(ctx context.Context, stream config.WorkstreamID) error {
	return d.driver().Request(ctx, stream)
}

func (d *finalReviewer) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.driver().Inspect(ctx, op)
}

func (d *finalReviewer) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.driver().Apply(ctx, op)
}

func (d *finalReviewer) PublishDraft(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, op coreadapter.Operation, report api.FinalReport) (string, error) {
	return d.driver().PublishDraft(ctx, cfg, stream, op, report)
}

func (d *finalReviewer) finalGate(ctx context.Context, stream config.WorkstreamID) (api.FinalReport, string, error) {
	return d.driver().FinalGate(ctx, stream)
}

func (s *Service) deliveryPresentation(ctx context.Context, raw string) (api.DeliveryPresentation, *api.APIError) {
	return s.delivery().DeliveryPresentation(ctx, raw)
}

func (s *Service) presentDelivery(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository) (api.DeliveryPresentation, *api.APIError) {
	return s.delivery().PresentDelivery(ctx, project, stream, repository)
}

func (s *Service) approveDelivery(ctx context.Context, raw string, req api.DeliveryDecision) (api.DeliveryApproval, *api.APIError) {
	return s.delivery().ApproveDelivery(ctx, raw, req)
}

func (s *Service) deliveryGate(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, description string) (api.DeliveryApproval, string, error) {
	return s.delivery().DeliveryGate(ctx, repository, stream, description)
}

type publisher struct {
	s          *Service
	repository *trace.Repository
}

func (d *publisher) driver() *delivery.Publisher {
	var c *delivery.Controller
	if d.s != nil {
		c = d.s.delivery()
	}
	return &delivery.Publisher{S: c, Repository: d.repository}
}

func (d *publisher) Pass(ctx context.Context) error { return d.driver().Pass(ctx) }

func (d *publisher) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.driver().Inspect(context.Background(), op)
}

func (d *publisher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.driver().Apply(ctx, op)
}

func (d *publisher) recordPublication(ctx context.Context, stream config.WorkstreamID, record api.DeliveryPublication) error {
	return d.driver().RecordPublication(ctx, stream, record)
}

func (s *Service) mergedOutside(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (api.MergeResponse, *api.APIError) {
	return s.delivery().MergedOutside(ctx, repository, stream)
}

func (s *Service) inbox(ctx context.Context) (api.InboxResponse, *api.APIError) {
	c := s.intake()
	c.DeliveryEntry = s.delivery().DeliveryEntry
	c.PublicationEntry = s.delivery().PublicationEntry
	return c.Inbox(ctx)
}

func (d *finalReviewer) incompleteChecks(stream config.WorkstreamID, k int, commit string) (int, delivery.FinalCheckRun, error) {
	return d.driver().IncompleteChecks(stream, k, commit)
}

type baseRefresher struct {
	s          *Service
	repository *trace.Repository
}

func (b *baseRefresher) driver() coreadapter.Reconciler {
	service.InspectForTest(b.s.core()).SetStoppedPullRequests(b.s.options.PullRequests)
	return service.BaseRefreshForTest(b.s.core(), b.repository, b.s.options.Reconciliation.Now, b.s.boundary)
}
func (b *baseRefresher) Pass(ctx context.Context) error {
	return b.driver().(interface{ Pass(context.Context) error }).Pass(ctx)
}
func (b *baseRefresher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return b.driver().Apply(ctx, op)
}
func conversationFixture(t *testing.T, key string) (Options, *config.Config) {
	o, cfg := servicetest.ConversationFixture(t, key)
	return Options{Options: o}, cfg
}
func runtimeFor(r *trace.Repository) *activeProject {
	return &activeProject{id: r.Project(), repository: r}
}

func (s *Service) resolveBase(ctx context.Context, cfg *config.Config, r *trace.Repository, w config.WorkstreamID, p workspace.Provider) (seal.Base, error) {
	return s.intake().ResolveBase(ctx, cfg, r, w, p)
}
func resolutionDocument(t *testing.T, r *trace.Repository, w config.WorkstreamID, id, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	fixturekit.Must(t, err)
	rev, err := intake.NextRevision(r, w, id)
	fixturekit.Must(t, err)
	fixturekit.Must(t, r.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: rev, Project: r.Project(), Workstream: w, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: "fixture"}, Path: path, Content: string(data)}}))
}

const publishUpstreamAction = "publish-upstream"
const deliveryReviewAction = "delivery-review"

func (b *baseRefresher) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return b.driver().Inspect(ctx, op)
}

func (s *Service) holding(r *trace.Repository) func(config.WorkstreamID, coreadapter.Operation) bool {
	service.InspectForTest(s.core()).SetStoppedPullRequests(s.options.PullRequests)
	return service.InspectForTest(s.core()).Holding(r)
}

func (f *foreman) RequestDrifts(ctx context.Context, due func(config.WorkstreamID) (string, error)) ([]config.WorkstreamID, error) {
	return service.DriftForTest(f.masons.s.core(), f.masons.repository).Request(ctx, due)
}
