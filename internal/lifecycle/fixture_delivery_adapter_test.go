package lifecycle_test

import (
	"context"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

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
func (d *finalReviewer) request(ctx context.Context, stream config.WorkstreamID) error {
	return d.driver().Request(ctx, stream)
}
func (d *finalReviewer) governing(ctx context.Context, stream config.WorkstreamID) (delivery.FinalReviewInput, seal.Seal, error) {
	return d.driver().Governing(ctx, stream)
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

func (s *Service) inbox(ctx context.Context) (api.InboxResponse, *api.APIError) {
	c := s.intake()
	c.DeliveryEntry = s.delivery().DeliveryEntry
	c.PublicationEntry = s.delivery().PublicationEntry
	return c.Inbox(ctx)
}

func (d *finalReviewer) followup(stream config.WorkstreamID, report api.FinalReport, revision int) (followup.Unit, bool, error) {
	return d.driver().Followup(stream, report, revision)
}
