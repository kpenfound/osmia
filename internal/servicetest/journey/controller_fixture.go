package journey

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

type Masons struct {
	S          *Service
	Cfg        *config.Config
	Repository *trace.Repository
}

func (m *Masons) Driver() *execution.Masons {
	var c *execution.Controller
	if m.S != nil {
		c = m.S.Execution()
	}
	return execution.NewMasons(c, m.Cfg, m.Repository)
}

type Foreman struct {
	*Masons
	NextDrift time.Time
}

func (f *Foreman) Driver() *execution.Foreman { return execution.NewForeman(f.Masons.Driver()) }

type Reviewers struct{ *Masons }

func (r *Reviewers) Driver() *execution.Reviewers { return execution.NewReviewers(r.Masons.Driver()) }

type Checkers struct{ *Masons }

func (c *Checkers) Driver() *execution.Checkers { return execution.NewCheckers(c.Masons.Driver()) }

type Rebaser struct{ *Foreman }

func (r Rebaser) Driver() execution.Rebaser { return execution.NewRebaser(r.Foreman.Driver()) }

type UnitWorkspaces struct {
	execution.UnitWorkspaces
	StreamWorkspaces intake.StreamWorkspaces
}

func NewUnitWorkspaces(cfg *config.Config, repo *trace.Repository) UnitWorkspaces {
	return UnitWorkspaces{execution.NewUnitWorkspaces(cfg, repo), intake.StreamWorkspaces{Cfg: cfg, Repository: repo, Directory: intake.UnitsDirectory}}
}

func (u UnitWorkspaces) Driver() execution.UnitWorkspaces { return u.UnitWorkspaces }

func (m *Masons) Pass(ctx context.Context) error { return m.Driver().Pass(ctx) }

func (m *Masons) Read(stream config.WorkstreamID) (execution.Building, bool, error) {
	return m.Driver().Read(stream)
}

func (m *Masons) Start(ctx context.Context, b execution.Building, unit string) (started, blocked bool, err error) {
	return m.Driver().Start(ctx, b, unit)
}

func (u UnitWorkspaces) Open(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, error) {
	return u.Driver().Open(ctx, stream, unit)
}

func (u UnitWorkspaces) Find(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, bool, error) {
	return u.Driver().Find(ctx, stream, unit)
}

func (c *Checkers) Pass(ctx context.Context) error { return c.Driver().Pass(ctx) }

func (c *Checkers) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return c.Driver().Inspect(ctx, op)
}

func (c *Checkers) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return c.Driver().Apply(ctx, op)
}

func (r *Reviewers) Pass(ctx context.Context) error { return r.Driver().Pass(ctx) }

func (r *Reviewers) One(ctx context.Context, stream config.WorkstreamID, unit string, state trace.WorkflowState, paused bool) error {
	return r.Driver().One(ctx, stream, unit, state, paused)
}

func (f *Foreman) Pass(ctx context.Context) error { return f.Driver().Pass(ctx) }

func (f *Foreman) Approval(stream config.WorkstreamID, unit string, revision int) (trace.Document, service.UnitReviewResult, bool, error) {
	return f.Driver().Approval(stream, unit, revision)
}

func (f *Foreman) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return f.Driver().Inspect(ctx, op)
}

func (f *Foreman) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return f.Driver().Apply(ctx, op)
}

func (f *Foreman) Current(ctx context.Context, stream config.WorkstreamID, in execution.LandInput, result service.UnitReviewResult) (string, error) {
	return f.Driver().Current(ctx, stream, in, result)
}

func (r Rebaser) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return r.Driver().Inspect(ctx, op)
}

func (r Rebaser) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return r.Driver().Apply(ctx, op)
}

func (m *Masons) Finish(ctx context.Context, b execution.Building, unit string) (moved, blocked bool, err error) {
	return m.Driver().Finish(ctx, b, unit)
}

func (m *Masons) PrepareUnitReview(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, service.UnitReviewIdentity, error) {
	return m.Driver().PrepareUnitReview(ctx, stream, unit)
}

type FinalReviewer struct {
	S          *Service
	Repository *trace.Repository
}

func (d *FinalReviewer) Driver() *delivery.FinalReviewer {
	var c *delivery.Controller
	if d.S != nil {
		c = d.S.Delivery()
	}
	return &delivery.FinalReviewer{S: c, Repository: d.Repository}
}

func (d *FinalReviewer) Pass(ctx context.Context) error { return d.Driver().Pass(ctx) }

func (d *FinalReviewer) Governing(ctx context.Context, stream config.WorkstreamID) (delivery.FinalReviewInput, seal.Seal, error) {
	return d.Driver().Governing(ctx, stream)
}

func (d *FinalReviewer) Request(ctx context.Context, stream config.WorkstreamID) error {
	return d.Driver().Request(ctx, stream)
}

func (d *FinalReviewer) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.Driver().Inspect(ctx, op)
}

func (d *FinalReviewer) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.Driver().Apply(ctx, op)
}

func (d *FinalReviewer) PublishDraft(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, op coreadapter.Operation, report api.FinalReport) (string, error) {
	return d.Driver().PublishDraft(ctx, cfg, stream, op, report)
}

func (d *FinalReviewer) FinalGate(ctx context.Context, stream config.WorkstreamID) (api.FinalReport, string, error) {
	return d.Driver().FinalGate(ctx, stream)
}

func (s *Service) DeliveryGate(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, description string) (api.DeliveryApproval, string, error) {
	return s.Delivery().DeliveryGate(ctx, repository, stream, description)
}

type Publisher struct {
	S          *Service
	Repository *trace.Repository
}

func (d *Publisher) Driver() *delivery.Publisher {
	var c *delivery.Controller
	if d.S != nil {
		c = d.S.Delivery()
	}
	return &delivery.Publisher{S: c, Repository: d.Repository}
}

func (d *Publisher) Pass(ctx context.Context) error { return d.Driver().Pass(ctx) }

func (d *Publisher) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.Driver().Inspect(context.Background(), op)
}

func (d *Publisher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.Driver().Apply(ctx, op)
}

func (d *Publisher) RecordPublication(ctx context.Context, stream config.WorkstreamID, record api.DeliveryPublication) error {
	return d.Driver().RecordPublication(ctx, stream, record)
}

type BaseRefresher struct {
	S          *Service
	Repository *trace.Repository
}

func (b *BaseRefresher) Driver() *lifecycle.BaseRefresher {
	service.InspectForTest(b.S.Core()).SetStoppedPullRequests(b.S.Options.PullRequests)
	return &lifecycle.BaseRefresher{S: b.S.Lifecycle(), Repository: b.Repository}
}

func (b *BaseRefresher) Pass(ctx context.Context) error { return b.Driver().Pass(ctx) }

func (b *BaseRefresher) ObserveDraftPullRequests(ctx context.Context) error {
	return b.Driver().ObserveDraftPullRequests(ctx)
}

func (b *BaseRefresher) ObserveLandedPullRequests(ctx context.Context) error {
	return b.Driver().ObserveLandedPullRequests(ctx)
}

func (b *BaseRefresher) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return b.Driver().Inspect(ctx, op)
}

func (b *BaseRefresher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return b.Driver().Apply(ctx, op)
}

func ConversationFixture(t *testing.T, key string) (Options, *config.Config) {
	o, cfg := servicetest.ConversationFixture(t, key)
	return Options{Options: o}, cfg
}

func ResolutionDocument(t *testing.T, r *trace.Repository, w config.WorkstreamID, id, path string, value any) {
	t.Helper()
	fixturekit.ResolutionDocument(t, r, w, id, path, value, fixturekit.DemoStart, intake.OwnerActor)
}

func (s *Service) Holding(r *trace.Repository) func(config.WorkstreamID, coreadapter.Operation) bool {
	service.InspectForTest(s.Core()).SetStoppedPullRequests(s.Options.PullRequests)
	return service.InspectForTest(s.Core()).Holding(r)
}

func (m *Masons) Bundle(ctx context.Context, stream config.WorkstreamID, unit string) (bundle.Mason, error) {
	return m.Driver().Bundle(ctx, stream, unit)
}

func (d *Publisher) Refuse(ctx context.Context, stream config.WorkstreamID, in delivery.PublishInput, reason string) (coreadapter.OperationResult, error) {
	return d.Driver().Refuse(ctx, stream, in, reason)
}

func (m *Masons) CandidateEvidence(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, service.UnitReviewIdentity, error) {
	return m.Driver().CandidateEvidence(ctx, stream, unit)
}
