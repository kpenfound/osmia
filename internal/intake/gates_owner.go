package intake

import (
	"context"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Controller) charterGate(repository *trace.Repository, stream config.WorkstreamID, question string) (*Gate, error) {
	p, found, err := FindCharterProposal(repository, stream, question)
	if err != nil || !found || p.State.Value != trace.CharterProposed {
		return nil, err
	}
	var options []jev.DecisionOption
	if rule := strings.TrimSpace(p.Proposal.Rule); rule != "" {
		options = append(options, jev.DecisionOption{Name: trace.CharterRatify, Description: "Ratify the rule as the manager wrote it: it is appended to the project's charter. Rule: " + limitText(rule, maxGateText)})
	}
	options = append(options, jev.DecisionOption{Name: trace.CharterDecline, Description: "Decline the rule: the charter is unchanged and the ruling stays a one-off answer."})
	project := repository.Project()
	g := &Gate{Kind: jev.KindCharterProposal, Project: project, Stream: stream, Key: jev.CharterKey(question),
		Label: fmt.Sprintf("the charter proposal of question %s of workstream %s", question, stream), Cause: p.Proposed.ID, Reached: p.Proposed.At, Options: options}
	g.State = func(ctx context.Context) (string, []jev.Source, error) {
		var b strings.Builder
		GateSection(&b, "Decision", fmt.Sprintf("The manager proposed a standing charter rule from the owner's ruling on question %s of workstream %s. Choose whether the owner ratifies it as written or declines it. Changing the rule's words is the owner's and is not offered here.", question, stream), 0)
		GateSection(&b, "Proposed rule", p.Proposal.Rule, 0)
		if p.Proposal.Amends != "" {
			GateSection(&b, "Amends", p.Proposal.Amends, 0)
		}
		GateSection(&b, "The owner's ruling it comes from", p.Proposal.OwnerResponse, maxGateText)
		GateSection(&b, "Charter", s.charterText(ctx, project), maxGateCharter)
		return b.String(), []jev.Source{{Kind: "charter-proposal", ID: p.Proposed.ID, Revision: p.Latest.Revision}}, nil
	}
	g.Apply = func(ctx context.Context, res jev.Result) *APIError {
		if res.Pick != trace.CharterDecline && (res.Pick != trace.CharterRatify || len(options) < 2) {
			return &APIError{Code: Conflict, Message: fmt.Sprintf("%q is not a decision offered on the charter proposal", res.Pick)}
		}
		note := fmt.Sprintf("Decided by Jev with confidence %.2f (judgment %s).", res.Confidence, res.Judgment)
		_, api := s.RecordCharterDecisionBy(ctx, project, stream, repository, question, CharterDecisionRequest{Decision: res.Pick, Note: note}, trace.JevActor, "jev-charter", Deciding{Judgment: res.Judgment})
		return api
	}
	g.Decision = func() (GateDecision, error) {
		now, found, err := FindCharterProposal(repository, stream, question)
		if err != nil || !found || now.State.Value == trace.CharterProposed {
			return GateDecision{}, err
		}
		return GateDecision{Recorded: true, ByJev: now.Proposal.DecidedBy == trace.DecidedByJev}, nil
	}
	return g, nil
}

// CharterProposalCard is a waiting charter proposal as the owner sees it, or
// nothing while Jev's judgment of it is pending or its pick is not applied.
// When the proposal came to the owner after Jev's judgment, the view carries
// Jev's result.
func (s *Controller) CharterProposalCard(repository *trace.Repository, p trace.CharterProposalState, view CharterProposalView) (CharterProposalView, bool) {
	if s.Jev == nil || p.State.Value != trace.CharterProposed {
		return view, true
	}
	g, err := s.charterGate(repository, p.Workstream, p.Proposal.Question)
	if err != nil || g == nil {
		return view, true
	}
	held, rec, jevView := s.holdsForJev(repository, *g)
	if held {
		return CharterProposalView{}, false
	}
	if jevView != nil {
		view.Jev = jevView
		view.Detail = rec
	}
	return view, true
}

func (s *Controller) deliveryCard(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (InboxEntry, bool, error) {
	e, open, err := s.DeliveryEntry(ctx, repository, stream)
	if err != nil || !open || s.Jev == nil || s.DeliveryGate == nil {
		return e, open, err
	}
	g, err := s.DeliveryGate(ctx, repository, stream, e)
	if err != nil {
		return InboxEntry{}, false, err
	}
	if !s.guardedCard(repository, &e, g) {
		return InboxEntry{}, false, nil
	}
	return e, true, nil
}

func (s *Controller) ownerGates(ctx context.Context, repository *trace.Repository, w trace.WorkstreamStatus) ([]Gate, error) {
	if !s.ownerJudged() {
		return nil, nil
	}
	var out []Gate
	if (w.State == AssembledState || w.State == DeliveredState) && s.DeliveryGate != nil && s.DeliveryEntry != nil {
		e, open, err := s.DeliveryEntry(ctx, repository, w.Workstream)
		if err != nil {
			return nil, err
		}
		if open {
			route, err := s.deliveryRouteGate(ctx, repository, w.Workstream)
			if err != nil {
				return nil, err
			}
			if route == nil {
				g, err := s.DeliveryGate(ctx, repository, w.Workstream, e)
				if err != nil {
					return nil, err
				}
				if g != nil {
					out = append(out, *g)
				}
			}
		}
	}
	for _, gate := range w.Gates {
		if gate.Kind != "charter" {
			continue
		}
		g, err := s.charterGate(repository, w.Workstream, gate.Reference)
		if err != nil {
			return nil, err
		}
		if g != nil {
			out = append(out, *g)
		}
	}
	if s.CancellationGates != nil {
		gates, err := s.CancellationGates(ctx, repository, w.Workstream)
		if err != nil {
			return nil, err
		}
		out = append(out, gates...)
	}
	return out, nil
}

func (s *Controller) ownerJudged() bool {
	return s.Jev != nil && s.Jev.Status().Mode != jev.ModeUnconfigured
}
