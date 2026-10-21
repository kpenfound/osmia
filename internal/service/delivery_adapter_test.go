package service

import (
	context "context"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	delivery "github.com/kpenfound/osmia/internal/delivery"
	seal "github.com/kpenfound/osmia/internal/seal"
	trace "github.com/kpenfound/osmia/internal/trace"
)

var finalTurnPrefix = delivery.FinalTurnPrefix

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

func (d *finalReviewer) governing(ctx context.Context, stream config.WorkstreamID) (finalReviewInput, seal.Seal, error) {
	return d.driver().Governing(ctx, stream)
}

func (d *finalReviewer) outcome(stream config.WorkstreamID, k int) (*coreadapter.OperationResult, error) {
	return d.driver().Outcome(stream, k)
}

func (d *finalReviewer) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.driver().Inspect(ctx, op)
}

func (d *finalReviewer) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.driver().Apply(ctx, op)
}

const ownPullRequestOutcomePath = delivery.OwnPullRequestOutcomePath
