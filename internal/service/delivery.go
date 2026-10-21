package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/trace"
)

const FinalReviewAction = delivery.FinalReviewAction

const finalReviewSubject = delivery.FinalReviewSubject

const finalRebaseDocument = delivery.FinalRebaseDocument

const finalReportPath = delivery.FinalReportPath

const finalReportDocument = delivery.FinalReportDocument

const finalReviewed = delivery.FinalReviewed

var finalReviewActor = delivery.FinalReviewActor

type finalReviewInput = delivery.FinalReviewInput

type FinalRebase = delivery.FinalRebase

const FindingBlocking = delivery.FindingBlocking

var finalTurnID = delivery.FinalTurnID

var decodeFinalReview = delivery.DecodeFinalReview

var countSeverity = delivery.CountSeverity

var finalRoundsWithBlocking = delivery.FinalRoundsWithBlocking

var latestFinalReport = delivery.LatestFinalReport

type FinalCheckRun = delivery.FinalCheckRun

type FinalCheckAttempt = delivery.FinalCheckAttempt

var finalCheckAttemptDocument = delivery.FinalCheckAttemptDocument

const deliveryPath = delivery.DeliveryPath

const deliveryDocument = delivery.DeliveryDocument

var deliveryDocuments = delivery.DeliveryDocuments

func (s *Service) deliveryPresentation(ctx context.Context, raw string) (DeliveryPresentation, *APIError) {
	return s.delivery().DeliveryPresentation(ctx, raw)
}

func (s *Service) approveDelivery(ctx context.Context, raw string, req DeliveryDecision) (DeliveryApproval, *APIError) {
	return s.delivery().ApproveDelivery(ctx, raw, req)
}

var publicationAction = delivery.PublicationAction

const PublishAction = delivery.PublishAction

const publicationSubject = delivery.PublicationSubject

const publicationPath = delivery.PublicationPath

const publicationDocument = delivery.PublicationDocument

const publicationOpened = delivery.PublicationOpened

type publishInput = delivery.PublishInput

const ownPullRequestOutcomeDocument = delivery.OwnPullRequestOutcomeDocument

const ownPullRequestOutcomeMerged = delivery.OwnPullRequestOutcomeMerged

const ownPullRequestOutcomeClosed = delivery.OwnPullRequestOutcomeClosed

type ownPullRequestOutcome = delivery.OwnPullRequestOutcome

const mergePath = delivery.MergePath

const mergeDocument = delivery.MergeDocument

func (s *Service) recordMerge(ctx context.Context, raw string) (MergeResponse, *APIError) {
	return s.delivery().RecordMerge(ctx, raw)
}

func (s *Service) publicationEntry(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error) {
	return s.delivery().PublicationEntry(repository, w)
}

var finalChecksEntry = delivery.FinalChecksEntry

func (s *Service) deliveryEntry(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	return s.delivery().DeliveryEntry(ctx, repository, stream)
}

const FinalReportTool = delivery.FinalReportTool
const FindingAdvisory = delivery.FindingAdvisory

var ownPullRequestOutcomeAt = delivery.OwnPullRequestOutcomeAt

var publications = delivery.Publications
