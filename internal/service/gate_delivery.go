package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) deliveryGate(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, e InboxEntry) (*intake.Gate, error) {
	review, _ := e.Answer.Body["review"].(int)
	revision, _ := e.Answer.Body["review_revision"].(int)
	commit, _ := e.Answer.Body["commit"].(string)
	draft, _ := e.Answer.Body["draft_hash"].(string)
	accept, _ := e.Answer.Body["accept_blocking"].(bool)
	if review == 0 || revision == 0 || commit == "" || draft == "" {
		return nil, nil
	}
	p, api := s.delivery().PresentDelivery(ctx, repository.Project(), stream, repository)
	if api != nil {
		if api.Code == Conflict {
			return nil, nil
		}
		return nil, fmt.Errorf("%s", api.Message)
	}
	if p.Approval != nil || p.Report.Review != review || p.ReviewRevision != revision || p.Report.Commit != commit || p.DraftHash != draft {
		return nil, nil
	}
	label := "Mark ready for review"
	switch {
	case p.Maintenance:
		label = "Update pull request"
	case accept:
		label = "Accept findings and mark ready for review"
	}
	project := repository.Project()
	g := &intake.Gate{Kind: jev.KindDeliveryApproval, Project: project, Stream: stream, Key: jev.DeliveryKey(review, revision, commit, draft),
		Label: fmt.Sprintf("the delivery of final review %d of workstream %s", review, stream), Cause: delivery.FinalReportDocument, Reached: e.OpenedAt,
		Options: []jev.DecisionOption{{Name: "approve", Description: label + ": the pull request gets the presented title and description and is marked ready for review."}}}
	g.State = func(ctx context.Context) (string, []jev.Source, error) {
		var b strings.Builder
		intake.GateSection(&b, "Decision", fmt.Sprintf("The %s card of workstream %s. The only decision offered is to approve it exactly as presented; editing the description, waiting or repairing the findings is the owner's.", label, stream), 0)
		intake.GateSection(&b, "Card", e.Question+"\nBlocked: "+e.Blocked, 0)
		intake.GateSection(&b, "Final review", finalReviewText(p.Report), 0)
		if p.Checks != nil {
			intake.GateSection(&b, "Checks", p.Checks.Summary, 0)
		}
		if p.Brief != nil {
			brief := p.Brief.Text
			if len(p.Brief.BeforeYouShip) > 0 {
				brief += "\nBefore you ship:\n- " + strings.Join(p.Brief.BeforeYouShip, "\n- ")
			}
			intake.GateSection(&b, "Chief of staff's delivery brief", brief, 8<<10)
		}
		intake.GateSection(&b, "Title", p.Title, 0)
		intake.GateSection(&b, "Description", p.Draft, 8<<10)
		if p.DraftPullRequest {
			intake.GateSection(&b, "Merge order", "Approving leaves the pull request a draft until its merge-order dependency has merged.", 0)
		}
		if doc, _, err := s.loadCharter(ctx, project); err == nil {
			intake.GateSection(&b, "Charter", doc.Content, 8<<10)
		}
		return b.String(), []jev.Source{{Kind: "final-review", ID: delivery.FinalReportDocument, Revision: revision}}, nil
	}
	g.Apply = func(ctx context.Context, res jev.Result) *APIError {
		if res.Pick != "approve" {
			return &APIError{Code: Conflict, Message: fmt.Sprintf("%q is not a delivery decision", res.Pick)}
		}
		_, api := s.delivery().ApproveDeliveryBy(ctx, string(stream), DeliveryDecision{Review: review, ReviewRevision: revision, Commit: commit, DraftHash: draft, AcceptBlocking: accept}, res.Judgment)
		return api
	}
	g.Decision = func() (intake.GateDecision, error) {
		_, _, approval, err := delivery.DeliveryDocuments(repository, stream)
		if err != nil || approval == nil {
			return intake.GateDecision{}, err
		}
		if approval.Review != review || approval.ReviewRevision != revision || approval.Commit != commit || approval.DraftHash != draft {
			return intake.GateDecision{}, nil
		}
		return intake.GateDecision{Recorded: true, ByJev: approval.DecidedBy == trace.DecidedByJev}, nil
	}
	return g, nil
}

func finalReviewText(r FinalReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Final review %d of commit %s. %s\n", r.Review, r.Commit, strings.TrimSpace(r.Summary))
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "- %s: %s\n", f.Severity, f.Describe())
	}
	return b.String()
}
