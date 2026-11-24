package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

const FinalReviewAction = delivery.FinalReviewAction

const finalReviewSubject = delivery.FinalReviewSubject

const finalReportPath = delivery.FinalReportPath

const finalReportDocument = delivery.FinalReportDocument

const finalReviewed = delivery.FinalReviewed

var finalReviewActor = delivery.FinalReviewActor

type finalReviewInput = delivery.FinalReviewInput

const FindingBlocking = delivery.FindingBlocking

var latestFinalReport = delivery.LatestFinalReport

type FinalCheckRun = delivery.FinalCheckRun

const deliveryPath = delivery.DeliveryPath

const deliveryDocument = delivery.DeliveryDocument

var deliveryDocuments = delivery.DeliveryDocuments

func (s *Service) deliveryPresentation(ctx context.Context, raw string) (DeliveryPresentation, *APIError) {
	return s.delivery().DeliveryPresentation(ctx, raw)
}

func (s *Service) approveDelivery(ctx context.Context, raw string, req DeliveryDecision) (DeliveryApproval, *APIError) {
	return s.delivery().ApproveDelivery(ctx, raw, req)
}

func (s *Service) approveAndLand(ctx context.Context, raw string, req DeliveryLandDecision) (DeliveryLandResponse, *APIError) {
	return s.delivery().ApproveAndLand(ctx, raw, req)
}

const PublishAction = delivery.PublishAction

const landDeliveryAction = delivery.LandDeliveryAction

const publicationSubject = delivery.PublicationSubject

const publicationPath = delivery.PublicationPath

const publicationDocument = delivery.PublicationDocument

const publicationOpened = delivery.PublicationOpened

const mergePath = delivery.MergePath

const mergeDocument = delivery.MergeDocument

func (s *Service) recordMerge(ctx context.Context, raw string) (api.MergeResponse, *APIError) {
	return s.delivery().RecordMerge(ctx, raw)
}

func (s *Service) publicationEntry(repository *trace.Repository, w trace.WorkstreamStatus) (InboxEntry, bool, error) {
	return s.delivery().PublicationEntry(repository, w)
}

func (s *Service) deliveryEntry(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	return s.delivery().DeliveryEntry(ctx, repository, stream)
}

func (s *Service) deliveryRouteGate(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (*intake.Gate, error) {
	return s.delivery().DeliveryRouteGate(ctx, repository, stream)
}

const FinalReportTool = delivery.FinalReportTool
