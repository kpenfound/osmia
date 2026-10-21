package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const finalFailed = delivery.FinalFailed

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

var finalSystemPrompt = delivery.FinalSystemPrompt

var finalChecksDocument = delivery.FinalChecksDocument

var finalChecksPath = delivery.FinalChecksPath

var finalCheckAttemptPath = delivery.FinalCheckAttemptPath

var descriptionHash = delivery.DescriptionHash

func draftDelivery(cfg *config.Config, r *trace.Repository, w config.WorkstreamID, report FinalReport) (string, string, error) {
	return delivery.New(delivery.Dependencies{PendingMergeOrderReferences: pendingMergeOrderReferences}).DraftDelivery(cfg, r, w, report)
}

const squashStyle = delivery.SquashStyle

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

const ownPullRequestOutcomePath = delivery.OwnPullRequestOutcomePath

var recordOwnPullRequestOutcome = delivery.RecordOwnPullRequestOutcome

func (d *finalReviewer) finalChecks(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, report FinalReport) (FinalCheckRun, error) {
	return d.driver().FinalChecks(ctx, cfg, stream, report)
}

func (d *finalReviewer) rebase(stream config.WorkstreamID, k int) (FinalRebase, bool, error) {
	return d.driver().Rebase(stream, k)
}

var incompleteFinalChecks = delivery.IncompleteFinalChecks
