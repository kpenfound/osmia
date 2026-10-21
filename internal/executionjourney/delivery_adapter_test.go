package executionjourney

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

const finalReviewSubject = delivery.FinalReviewSubject

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

func (d *finalReviewer) request(ctx context.Context, stream config.WorkstreamID) error {
	return d.driver().Request(ctx, stream)
}

func (d *finalReviewer) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.driver().Inspect(ctx, op)
}

func (d *finalReviewer) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.driver().Apply(ctx, op)
}

var finalReviewNotice = delivery.FinalReviewNotice

var latestFinalReport = delivery.LatestFinalReport

func (d *finalReviewer) finalGate(ctx context.Context, stream config.WorkstreamID) (FinalReport, string, error) {
	return d.driver().FinalGate(ctx, stream)
}

type FinalCheckRun = delivery.FinalCheckRun

var finalChecksDocument = delivery.FinalChecksDocument

var finalChecksPath = delivery.FinalChecksPath

type FinalCheckAttempt = delivery.FinalCheckAttempt

var finalCheckAttemptDocument = delivery.FinalCheckAttemptDocument

var finalCheckDelay = delivery.FinalCheckDelay

const deliveryPath = delivery.DeliveryPath

const deliveryDocument = delivery.DeliveryDocument

const deliverySubject = delivery.DeliverySubject

var descriptionHash = delivery.DescriptionHash

var boundSubject = delivery.BoundSubject

func (s *Service) deliveryPresentation(ctx context.Context, raw string) (DeliveryPresentation, *APIError) {
	return s.delivery().DeliveryPresentation(ctx, raw)
}

func (s *Service) presentDelivery(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository) (DeliveryPresentation, *APIError) {
	return s.delivery().PresentDelivery(ctx, project, stream, repository)
}

func (s *Service) approveDelivery(ctx context.Context, raw string, req DeliveryDecision) (DeliveryApproval, *APIError) {
	return s.delivery().ApproveDelivery(ctx, raw, req)
}

func (s *Service) deliveryGate(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, description string) (DeliveryApproval, string, error) {
	return s.delivery().DeliveryGate(ctx, repository, stream, description)
}

var errNoPullRequestCredential = delivery.ErrNoPullRequestCredential

const PublishAction = delivery.PublishAction

const publicationSubject = delivery.PublicationSubject

const publicationPath = delivery.PublicationPath

const publicationDocument = delivery.PublicationDocument

const publicationPushing = delivery.PublicationPushing

const publicationOpened = delivery.PublicationOpened

var publishIDs = delivery.PublishIDs

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

var publicationRefused = delivery.PublicationRefused

func (d *publisher) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return d.driver().Inspect(context.Background(), op)
}

func (d *publisher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return d.driver().Apply(ctx, op)
}

var deliveryTitle = delivery.DeliveryTitle

var publications = delivery.Publications

func (d *publisher) recordPublication(ctx context.Context, stream config.WorkstreamID, record DeliveryPublication) error {
	return d.driver().RecordPublication(ctx, stream, record)
}

const ownPullRequestOutcomeMerged = delivery.OwnPullRequestOutcomeMerged

const ownPullRequestOutcomeClosed = delivery.OwnPullRequestOutcomeClosed

var ownPullRequestOutcomeAt = delivery.OwnPullRequestOutcomeAt

var ownPullRequestFinished = delivery.OwnPullRequestFinished

const mergePath = delivery.MergePath

func (s *Service) mergedOutside(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (MergeResponse, *APIError) {
	return s.delivery().MergedOutside(ctx, repository, stream)
}

var deliveryMerge = delivery.ReadDeliveryMerge

var finalChecksEntry = delivery.FinalChecksEntry

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

const FindingAdvisory = delivery.FindingAdvisory

func (s *Service) inbox(ctx context.Context) (InboxResponse, *APIError) {
	c := s.intake()
	c.DeliveryEntry = s.delivery().DeliveryEntry
	c.PublicationEntry = s.delivery().PublicationEntry
	return c.Inbox(ctx)
}

func (d *finalReviewer) incompleteChecks(stream config.WorkstreamID, k int, commit string) (int, FinalCheckRun, error) {
	return d.driver().IncompleteChecks(stream, k, commit)
}

func (d *finalReviewer) finalChecks(ctx context.Context, cfg *config.Config, stream config.WorkstreamID, report FinalReport) (FinalCheckRun, error) {
	return d.driver().FinalChecks(ctx, cfg, stream, report)
}

var feedChecks = service.TestFeedChecks
