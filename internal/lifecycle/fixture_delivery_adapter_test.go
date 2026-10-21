package lifecycle_test

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const finalRebasePath = delivery.FinalRebasePath
const finalRebaseDocument = delivery.FinalRebaseDocument
const finalFailed = delivery.FinalFailed

var reportBlocking = delivery.ReportBlocking

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

func (d *finalReviewer) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.driver().Inspect(ctx, op)
}
func (d *finalReviewer) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.driver().Apply(ctx, op)
}

var latestFinalReport = delivery.LatestFinalReport

func (d *finalReviewer) finalGate(ctx context.Context, stream config.WorkstreamID) (FinalReport, string, error) {
	return d.driver().FinalGate(ctx, stream)
}

type FinalCheckRun = delivery.FinalCheckRun

var finalChecksDocument = delivery.FinalChecksDocument

func (s *Service) deliveryPresentation(ctx context.Context, raw string) (DeliveryPresentation, *APIError) {
	return s.delivery().DeliveryPresentation(ctx, raw)
}
func (s *Service) approveDelivery(ctx context.Context, raw string, req DeliveryDecision) (DeliveryApproval, *APIError) {
	return s.delivery().ApproveDelivery(ctx, raw, req)
}
func (s *Service) deliveryGate(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, description string) (DeliveryApproval, string, error) {
	return s.delivery().DeliveryGate(ctx, repository, stream, description)
}

const PublishAction = delivery.PublishAction
const publicationPath = delivery.PublicationPath
const publicationDocument = delivery.PublicationDocument
const publicationOpened = delivery.PublicationOpened

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

var decodePublish = delivery.DecodePublish

func (d *publisher) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.driver().Inspect(context.Background(), op)
}
func (d *publisher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.driver().Apply(ctx, op)
}

var publications = delivery.Publications

func (d *publisher) recordPublication(ctx context.Context, stream config.WorkstreamID, record DeliveryPublication) error {
	return d.driver().RecordPublication(ctx, stream, record)
}

const ownPullRequestOutcomeMerged = delivery.OwnPullRequestOutcomeMerged

var ownPullRequestOutcomeAt = delivery.OwnPullRequestOutcomeAt
var ownPullRequestFinished = delivery.OwnPullRequestFinished

const mergePath = delivery.MergePath

type FinalRebase = delivery.FinalRebase
type finalReviewInput = delivery.FinalReviewInput

const finalReportDocument = delivery.FinalReportDocument
const finalReportPath = delivery.FinalReportPath
const finalReviewed = delivery.FinalReviewed

var finalReviewActor = delivery.FinalReviewActor
var finalTurnID = delivery.FinalTurnID
var decodeFinalReview = delivery.DecodeFinalReview

const FinalReviewAction = delivery.FinalReviewAction
const FindingBlocking = delivery.FindingBlocking

func (s *Service) inbox(ctx context.Context) (InboxResponse, *APIError) {
	c := s.intake()
	c.DeliveryEntry = s.delivery().DeliveryEntry
	c.PublicationEntry = s.delivery().PublicationEntry
	return c.Inbox(ctx)
}

func (d *finalReviewer) followup(stream config.WorkstreamID, report FinalReport, revision int) (followup.Unit, bool, error) {
	return d.driver().Followup(stream, report, revision)
}

var finalSystemPrompt = delivery.FinalSystemPrompt
var finalPrompt = delivery.FinalPrompt

func (d *finalReviewer) incompleteChecks(stream config.WorkstreamID, k int, commit string) (int, FinalCheckRun, error) {
	return d.driver().IncompleteChecks(stream, k, commit)
}

const ownPullRequestOutcomePath = delivery.OwnPullRequestOutcomePath

var recordOwnPullRequestOutcome = delivery.RecordOwnPullRequestOutcome
