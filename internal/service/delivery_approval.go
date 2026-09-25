package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const deliveryPath = "final/delivery.json"
const deliveryDocument = "final-delivery"
const deliverySubject = "delivery-owner"

// DeliveryPresentation is the final review and the description presented to
// the owner. Criteria with a gap precede criteria with evidence. A delivered
// workstream presents the report, approval and publication it was delivered
// with, and no draft.
type DeliveryPresentation struct {
	Project        config.ProjectID    `json:"project"`
	Workstream     config.WorkstreamID `json:"workstream"`
	Report         FinalReport         `json:"report"`
	ReviewRevision int                 `json:"review_revision"`
	Draft          string              `json:"draft"`
	DraftHash      string              `json:"draft_hash"`
	Approval       *DeliveryApproval   `json:"approval,omitempty"`
	// Publication is the latest record of publishing an approval, and
	// Delivered whether the workstream is delivered.
	Publication *DeliveryPublication `json:"publication,omitempty"`
	Delivered   bool                 `json:"delivered,omitempty"`
}

// DeliveryApproval preserves the exact text and reviewed identity the owner
// approved. DescriptionHash allows publication to check the text it will send.
type DeliveryApproval struct {
	Review          int       `json:"review"`
	ReviewRevision  int       `json:"review_revision"`
	Commit          string    `json:"commit"`
	Seal            int       `json:"seal"`
	SpecHash        string    `json:"spec_hash"`
	Spec            int       `json:"spec"`
	Plan            int       `json:"plan"`
	Charter         int       `json:"charter"`
	DraftHash       string    `json:"draft_hash"`
	Description     string    `json:"description"`
	DescriptionHash string    `json:"description_hash"`
	At              time.Time `json:"at"`
}

// DeliveryDecision approves the draft as shown, or supplies the owner's edit.
// The review identity and draft hash are required even when the text is edited.
type DeliveryDecision struct {
	Review         int     `json:"review"`
	ReviewRevision int     `json:"review_revision"`
	Commit         string  `json:"commit"`
	DraftHash      string  `json:"draft_hash"`
	Description    *string `json:"description,omitempty"`
}

func descriptionHash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func deliveryDocuments(repository *trace.Repository, stream config.WorkstreamID) (trace.Document, trace.Document, *DeliveryApproval, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return trace.Document{}, trace.Document{}, nil, err
	}
	var review, decision trace.Document
	for _, d := range docs {
		switch d.ID {
		case finalReportDocument:
			review = d
		case deliveryDocument:
			decision = d
		}
	}
	if decision.Revision == 0 {
		return review, decision, nil, nil
	}
	var approval DeliveryApproval
	if err := json.Unmarshal([]byte(decision.Content), &approval); err != nil {
		return review, decision, nil, err
	}
	return review, decision, &approval, nil
}

// draftDelivery uses recorded evidence and landed-unit records, so repeated
// presentations of one trace produce the same description.
func draftDelivery(repository *trace.Repository, stream config.WorkstreamID, report FinalReport) (string, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(report.Summary)
	if title == "" {
		title = "Workstream " + string(stream)
	}
	var lines []string
	lines = append(lines, "# "+title, "", "## Final review", "", fmt.Sprintf("Reviewed commit: `%s`", report.Commit), "")
	for _, c := range report.Criteria {
		line := "- **" + c.Criterion + "** " + c.Text + " — "
		if c.Gap != "" {
			line += "Gap: " + c.Gap
		} else {
			line += "Evidence: " + c.Evidence
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", "## Landed units", "")
	latest := map[string]UnitLanding{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "units/") && strings.HasSuffix(d.Path, "/landing.json") {
			var landing UnitLanding
			if err := json.Unmarshal([]byte(d.Content), &landing); err != nil {
				return "", err
			}
			latest[landing.Unit] = landing
		}
	}
	var landings []string
	for _, landing := range latest {
		landings = append(landings, fmt.Sprintf("- %s: `%s` (%s)", landing.Unit, landing.Commit, strings.Join(landing.Criteria, ", ")))
	}
	slices.Sort(landings)
	if len(landings) == 0 {
		landings = []string{"- No landing records are recorded."}
	}
	lines = append(lines, landings...)
	return strings.Join(lines, "\n") + "\n", nil
}

func (s *Service) deliveryPresentation(ctx context.Context, raw string) (DeliveryPresentation, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return DeliveryPresentation{}, api
	}
	return s.presentDelivery(ctx, project, stream, repository)
}

// presentDelivery builds the presentation of one workstream of the project.
func (s *Service) presentDelivery(ctx context.Context, project config.ProjectID, stream config.WorkstreamID, repository *trace.Repository) (DeliveryPresentation, *APIError) {
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot read feature state"}
	}
	published, err := publications(repository, stream)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot read publication records"}
	}
	var publication *DeliveryPublication
	if len(published) > 0 {
		publication = &published[len(published)-1]
	}
	if feature.Value == DeliveredState {
		report, _, err := latestFinalReport(repository, stream)
		if err != nil {
			return DeliveryPresentation{}, &APIError{Internal, "cannot read final review"}
		}
		review, _, approval, err := deliveryDocuments(repository, stream)
		if err != nil {
			return DeliveryPresentation{}, &APIError{Internal, "cannot read delivery records"}
		}
		return DeliveryPresentation{Project: project, Workstream: stream, Report: report, ReviewRevision: review.Revision, Approval: approval, Publication: publication, Delivered: true}, nil
	}
	if feature.Value != AssembledState {
		return DeliveryPresentation{}, &APIError{Conflict, "the workstream is not assembled"}
	}
	report, reason, err := (&finalReviewer{s: s, repository: repository}).finalGate(ctx, stream)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot read final review"}
	}
	// An unshown criterion is part of the report the owner must see, even
	// though that report cannot authorize delivery yet.
	if reason != "" && !strings.Contains(reason, "unresolved gap") {
		return DeliveryPresentation{}, &APIError{Conflict, reason}
	}
	review, _, approval, err := deliveryDocuments(repository, stream)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot read delivery records"}
	}
	if review.Revision == 0 {
		return DeliveryPresentation{}, &APIError{Conflict, "no final review is recorded"}
	}
	slices.SortStableFunc(report.Criteria, func(a, b FinalCriterion) int {
		if (a.Gap != "") == (b.Gap != "") {
			return 0
		}
		if a.Gap != "" {
			return -1
		}
		return 1
	})
	draft, err := draftDelivery(repository, stream, report)
	if err != nil {
		return DeliveryPresentation{}, &APIError{Internal, "cannot draft the description from trace records"}
	}
	if approval != nil && (approval.Review != report.Review || approval.ReviewRevision != review.Revision || approval.Commit != report.Commit || approval.SpecHash != report.SpecHash || approval.Spec != report.Spec || approval.Plan != report.Plan || approval.Charter != report.Charter || approval.Seal != report.Seal) {
		approval = nil
	}
	return DeliveryPresentation{Project: project, Workstream: stream, Report: report, ReviewRevision: review.Revision, Draft: draft, DraftHash: descriptionHash(draft), Approval: approval, Publication: publication}, nil
}

func (s *Service) approveDelivery(ctx context.Context, raw string, req DeliveryDecision) (DeliveryApproval, *APIError) {
	presented, api := s.deliveryPresentation(ctx, raw)
	if api != nil {
		return DeliveryApproval{}, api
	}
	if presented.Delivered {
		return DeliveryApproval{}, &APIError{Conflict, "the workstream is delivered"}
	}
	if req.Review != presented.Report.Review || req.ReviewRevision != presented.ReviewRevision || req.Commit != presented.Report.Commit || req.DraftHash != presented.DraftHash {
		return DeliveryApproval{}, &APIError{Conflict, "the final report or draft description changed; read the current presentation"}
	}
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return DeliveryApproval{}, api
	}
	if _, reason, err := (&finalReviewer{s: s, repository: repository}).finalGate(ctx, stream); err != nil {
		return DeliveryApproval{}, &APIError{Internal, "cannot validate final review"}
	} else if reason != "" {
		return DeliveryApproval{}, &APIError{Conflict, reason}
	}
	description := presented.Draft
	if req.Description != nil {
		description = *req.Description
	}
	if strings.TrimSpace(description) == "" {
		return DeliveryApproval{}, &APIError{Validation, "description must not be empty"}
	}
	review, prior, existing, err := deliveryDocuments(repository, stream)
	if err != nil {
		return DeliveryApproval{}, &APIError{Internal, "cannot read delivery records"}
	}
	// Approving again after a refused publication records a new approval,
	// which asks for another publication.
	if existing != nil {
		refused, err := publicationRefused(repository, stream, prior.Revision)
		if err != nil {
			return DeliveryApproval{}, &APIError{Internal, "cannot read publication records"}
		}
		if refused {
			existing = nil
		}
	}
	if existing != nil && req.Description == nil && existing.ReviewRevision == review.Revision && existing.Commit == req.Commit && existing.DraftHash == req.DraftHash {
		return *existing, nil
	}
	if existing != nil && existing.ReviewRevision == review.Revision && existing.Commit == req.Commit && existing.Description == description && existing.DraftHash == req.DraftHash {
		return *existing, nil
	}
	r := presented.Report
	approval := DeliveryApproval{Review: r.Review, ReviewRevision: review.Revision, Commit: r.Commit, Seal: r.Seal, SpecHash: r.SpecHash, Spec: r.Spec, Plan: r.Plan, Charter: r.Charter, DraftHash: req.DraftHash, Description: description, DescriptionHash: descriptionHash(description), At: s.now()}
	content, err := json.Marshal(approval)
	if err != nil {
		return DeliveryApproval{}, &APIError{Internal, "cannot encode delivery approval"}
	}
	state, err := repository.Workflow(stream, deliverySubject)
	if err != nil {
		return DeliveryApproval{}, &APIError{Internal, "cannot read delivery state"}
	}
	id := fmt.Sprintf("delivery-owner-%d", prior.Revision+1)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: deliveryDocument, Revision: prior.Revision + 1, Project: project, Workstream: stream, At: approval.At, Actor: ownerActor, Cause: id}, Path: deliveryPath, Content: string(content)}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: ownerHeader(id, project, stream, "delivery-approval", approval.At), Subject: deliverySubject, From: state.Value, To: fmt.Sprintf("approved-%d", prior.Revision+1), Reason: fmt.Sprintf("the owner approved description %s for final review %d at %s", approval.DescriptionHash, approval.Review, approval.Commit)}}
	if _, err := repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return DeliveryApproval{}, &APIError{Conflict, "delivery decision changed; read the current presentation"}
		}
		return DeliveryApproval{}, &APIError{Internal, "cannot record delivery approval"}
	}
	return approval, nil
}

// deliveryGate checks the approval against the current review and the exact
// description publication proposes to send.
func (s *Service) deliveryGate(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, description string) (DeliveryApproval, string, error) {
	report, reason, err := (&finalReviewer{s: s, repository: repository}).finalGate(ctx, stream)
	if err != nil || reason != "" {
		return DeliveryApproval{}, reason, err
	}
	review, _, approval, err := deliveryDocuments(repository, stream)
	if err != nil {
		return DeliveryApproval{}, "", err
	}
	if approval == nil {
		return DeliveryApproval{}, "no owner delivery approval is recorded", nil
	}
	if approval.Review != report.Review || approval.ReviewRevision != review.Revision || approval.Commit != report.Commit || approval.Seal != report.Seal || approval.SpecHash != report.SpecHash || approval.Spec != report.Spec || approval.Plan != report.Plan || approval.Charter != report.Charter {
		return *approval, "the owner approval is stale; review and approve the current branch", nil
	}
	if approval.DescriptionHash != descriptionHash(approval.Description) || approval.Description != description {
		return *approval, "the description differs from what the owner approved", nil
	}
	return *approval, "", nil
}
