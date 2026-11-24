package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	cancelApproveClose = "approve-close"
	cancelApproveDraft = "approve-draft"
	cancelReject       = "reject"
)

func (s *Service) cancellationGates(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) ([]intake.Gate, error) {
	changes, err := readChanges(repository, stream)
	if err != nil {
		return nil, err
	}
	var out []intake.Gate
	for _, c := range changes {
		if c.Request.Kind != "cancel" || c.State != "presented" || c.Decision != nil || c.Host != stream {
			continue
		}
		out = append(out, s.cancellationGate(repository, stream, c, changePresentedAt(repository, stream, c)))
	}
	return out, nil
}

func changePresentedAt(repository *trace.Repository, stream config.WorkstreamID, c lifecycle.WorkgroupChange) time.Time {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return time.Time{}
	}
	var at time.Time
	for _, d := range docs {
		if d.ID == workgroupChangeDocument(c.ID) && d.Revision == c.Revision {
			at = d.At
		}
	}
	return at
}

func (s *Service) cancellationGate(repository *trace.Repository, stream config.WorkstreamID, c lifecycle.WorkgroupChange, reached time.Time) intake.Gate {
	project := repository.Project()
	scope := "the whole workgroup"
	if c.Request.Scope == "workstream" {
		scope = "member workstream " + string(c.Request.Workstream)
	}
	g := intake.Gate{Kind: jev.KindCancellation, Project: project, Stream: stream, Key: jev.CancellationKey(c.ID, c.Revision),
		Label: fmt.Sprintf("the cancellation %s of workgroup %s", c.ID, c.Workgroup), Cause: workgroupChangeDocument(c.ID), Reached: reached,
		Options: []jev.DecisionOption{
			{Name: cancelApproveClose, Description: "Approve the cancellation and close its open pull requests. Merged work stays as it is."},
			{Name: cancelApproveDraft, Description: "Approve the cancellation and leave its open pull requests as drafts. Merged work stays as it is."},
			{Name: cancelReject, Description: "Reject the cancellation: the work continues as approved."}}}
	g.State = func(ctx context.Context) (string, []jev.Source, error) {
		var b strings.Builder
		intake.GateSection(&b, "Decision", fmt.Sprintf("The owner asked to cancel %s, and the impact summary below is presented for the owner's decision. Choose whether the owner approves it, and what happens to the open pull requests, or rejects it.", scope), 0)
		intake.GateSection(&b, "Impact summary", c.Summary(), 16<<10)
		if doc, _, err := s.loadCharter(ctx, project); err == nil {
			intake.GateSection(&b, "Charter", doc.Content, 8<<10)
		}
		return b.String(), []jev.Source{{Kind: "workgroup-change", ID: workgroupChangeDocument(c.ID), Revision: c.Revision}}, nil
	}
	g.Apply = func(ctx context.Context, res jev.Result) *APIError {
		d := lifecycle.ChangeDecision{Revision: c.Revision, DecidedBy: trace.DecidedByJev, Judgment: res.Judgment}
		switch res.Pick {
		case cancelApproveClose:
			d.Decision, d.PullRequests = "approve", "close"
		case cancelApproveDraft:
			d.Decision, d.PullRequests = "approve", "draft"
		case cancelReject:
			d.Decision = "reject"
		default:
			return &APIError{Code: Conflict, Message: fmt.Sprintf("%q is not a decision on a cancellation", res.Pick)}
		}
		_, api := s.lifecycle().DecideChange(ctx, string(stream), c.ID, d)
		return api
	}
	g.Decision = func() (intake.GateDecision, error) {
		changes, err := readChanges(repository, stream)
		if err != nil {
			return intake.GateDecision{}, err
		}
		for _, now := range changes {
			if now.ID == c.ID && now.Decision != nil {
				return intake.GateDecision{Recorded: true, ByJev: now.Decision.DecidedBy == trace.DecidedByJev}, nil
			}
		}
		return intake.GateDecision{}, nil
	}
	return g
}
