package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/trace"
)

func notLandingReason(cfg *config.Config) string {
	if cfg.HasProject() && !cfg.Project.Lands() {
		return fmt.Sprintf("the project's delivery is %q, so it delivers by pull request and never lands on its base branch; set delivery to %q to land", cfg.Project.Delivery, config.DeliveryJev)
	}
	return ""
}

// ApproveAndLand records the owner's approval to land the presented workstream
// and lands it through LandWorkstream, whether or not Jev is configured. It is
// refused as a conflict, with nothing recorded or pushed, when the project does
// not land, when the presentation the request pins is stale, and under the
// preconditions that refuse ApproveDelivery and LandWorkstream: blocking
// findings the request does not accept, a dependency still waiting to merge,
// and checks that did not pass on the pushed commit. Once accepted, the
// response reports what landing did: landed, held by the router, fell back to
// the draft pull request after the upstream refused the push, base moved, or
// pending its reconciliation. None of those raises another delivery card.
func (s *Controller) ApproveAndLand(ctx context.Context, raw string, req DeliveryLandDecision) (DeliveryLandResponse, *APIError) {
	project, stream, repository, api := s.ConversationTrace(raw)
	if api != nil {
		return DeliveryLandResponse{}, api
	}
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: string(project), Workstream: string(stream)})
	cfg := s.About(repository)
	if reason := notLandingReason(cfg); reason != "" {
		return DeliveryLandResponse{}, &APIError{Code: Conflict, Message: reason}
	}
	presented, api := s.PresentDelivery(ctx, project, stream, repository)
	if api != nil {
		return DeliveryLandResponse{}, api
	}
	if presented.Delivered && !presented.Maintenance {
		return DeliveryLandResponse{}, &APIError{Code: Conflict, Message: "the workstream is delivered"}
	}
	if req.Review != presented.Report.Review || req.ReviewRevision != presented.ReviewRevision || req.Commit != presented.Report.Commit || req.DraftHash != presented.DraftHash {
		return DeliveryLandResponse{}, &APIError{Code: Conflict, Message: "the final report or draft description changed; read the current presentation"}
	}
	if _, reason, err := s.landingTarget(ctx, repository, cfg, stream, req.AcceptBlocking, true); err != nil {
		return DeliveryLandResponse{}, &APIError{Code: Internal, Message: "cannot validate the landing"}
	} else if reason != "" {
		return DeliveryLandResponse{}, &APIError{Code: Conflict, Message: reason}
	}
	approval, api := s.recordLandApproval(ctx, project, stream, repository, presented, req)
	if api != nil {
		return DeliveryLandResponse{}, api
	}
	result, err := s.LandWorkstream(ctx, repository, stream, LandRequest{AcceptBlocking: req.AcceptBlocking})
	if err != nil {
		return DeliveryLandResponse{}, &APIError{Code: Internal, Message: "cannot land the workstream: " + err.Error()}
	}
	if result.Outcome == LandingRefusedBeforePush {
		return DeliveryLandResponse{}, &APIError{Code: Conflict, Message: result.Reason}
	}
	return DeliveryLandResponse{Approval: approval, Outcome: string(result.Outcome), Reason: result.Reason, Commit: result.Commit}, nil
}

func (s *Controller) recordLandApproval(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository, presented DeliveryPresentation, req DeliveryLandDecision) (DeliveryApproval, *APIError) {
	blocking := ReportBlocking(presented.Report)
	review, prior, existing, err := DeliveryDocuments(repository, stream)
	if err != nil {
		return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot read delivery records"}
	}
	if existing != nil {
		refused, err := PublicationRefused(repository, stream, prior.Revision)
		if err != nil {
			return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot read publication records"}
		}
		if !refused && existing.Review == req.Review && existing.ReviewRevision == review.Revision && existing.Commit == req.Commit && existing.DraftHash == req.DraftHash && existing.Style == presented.Style && slices.Equal(existing.AcceptedBlocking, blocking) {
			if existing.Route != DeliveryRouteLand {
				return DeliveryApproval{}, &APIError{Code: Conflict, Message: "marking the pull request ready for review is already approved for this report; it delivers the workstream"}
			}
			return *existing, nil
		}
	}
	r := presented.Report
	approval := DeliveryApproval{Review: r.Review, ReviewRevision: review.Revision, Commit: r.Commit, Seal: r.Seal, SpecHash: r.SpecHash, Spec: r.Spec, Plan: r.Plan, Charter: r.Charter, DraftHash: req.DraftHash, Title: presented.Title,
		Description: presented.Draft, DescriptionHash: DescriptionHash(presented.Draft), Messages: slices.Clone(presented.Messages), Style: presented.Style, At: s.Now(), AcceptedBlocking: blocking, Route: DeliveryRouteLand}
	content, err := json.Marshal(approval)
	if err != nil {
		return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot encode delivery approval"}
	}
	state, err := repository.Workflow(stream, DeliverySubject)
	if err != nil {
		return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot read delivery state"}
	}
	id := fmt.Sprintf("delivery-owner-%d", prior.Revision+1)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: DeliveryDocument, Revision: prior.Revision + 1, Project: project, Workstream: stream, At: approval.At, Actor: ownerActor, Cause: id}, Path: DeliveryPath, Content: string(content)}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: ownerHeader(id, project, stream, "delivery-approval", approval.At), Subject: DeliverySubject, From: state.Value, To: fmt.Sprintf("approved-%d", prior.Revision+1), Reason: fmt.Sprintf("the owner approved landing final review %d at %s on the base branch", approval.Review, approval.Commit)}}
	if _, err := repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return DeliveryApproval{}, &APIError{Code: Conflict, Message: "delivery decision changed; read the current presentation"}
		}
		return DeliveryApproval{}, &APIError{Code: Internal, Message: "cannot record delivery approval"}
	}
	return approval, nil
}

func landingPending(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	l := &Lander{Repository: repository}
	_, _, outcome, found, err := l.latest(stream)
	return found && outcome == nil, err
}
