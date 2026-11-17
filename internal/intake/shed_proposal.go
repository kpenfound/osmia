package intake

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// ShedProposal is a proposal of the chief of staff to ask the architect for a
// redraft of the spec and the plan, or to run another round of debate, after
// debate concluded.
type ShedProposal struct {
	// Redraft says the proposal is a redraft; otherwise it is another round.
	Redraft bool
	Note    string
	Turn    string
	Actor   trace.Actor
	At      time.Time
}

// proposer names who a proposal's records are attributed to.
func proposer(actor trace.Actor) string {
	if actor == trace.JevActor {
		return "Jev"
	}
	return "the chief of staff"
}

// ShedProposalExit returns the exit of the proposal on the workstream whose id
// is raw. Leaving the decision to the owner is the default: the owner asks for
// a redraft or another round, and the chief of staff does so only by a
// departure the router takes at the strict bar. Entering a target records what
// the owner's command records, with the chief of staff's note.
func (s *Controller) ShedProposalExit(raw string, in ShedProposal) (router.Exit, *APIError) {
	o, api := s.ShedAction(raw, InShedState)
	if api != nil {
		return router.Exit{}, api
	}
	kind, n, ok := ShedState(o.Shed.Value)
	switch {
	case o.Skipped:
		return router.Exit{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the owner skipped debate on workstream %s; no further round or redraft runs", o.Stream)}
	case !ok || kind != "concluded":
		return router.Exit{}, &APIError{Code: Conflict, Message: fmt.Sprintf("debate on workstream %s has not concluded; a redraft or another round is proposed once it has", o.Stream)}
	}
	name, enters, description := TargetDebateRound, fmt.Sprintf("more-%d", n), "Run another round of debate on the standing dissent."
	if in.Redraft {
		name, enters, description = TargetRedraft, fmt.Sprintf("redraft-%d", n), "Ask the architect for a redraft that answers the standing dissent."
	}
	redrafts, err := shed.Redrafts(o.Repository, o.Stream)
	if err != nil {
		return router.Exit{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the requests of workstream %s; check the trace repository", o.Stream)}
	}
	if in.Redraft && slices.ContainsFunc(redrafts, func(r shed.Redraft) bool { return r.Round == n }) {
		return router.Exit{}, &APIError{Code: Conflict, Message: fmt.Sprintf("a redraft after round %d of workstream %s is already asked for", n, o.Stream)}
	}
	open, err := Dissent(o.Repository, o.Stream)
	if err != nil {
		return router.Exit{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the dissent record of workstream %s; check the trace repository", o.Stream)}
	}
	repo, stream, note := o.Repository, o.Stream, in.Note
	cause := fmt.Sprintf("shed-proposal-%s-%d-%d", name, n, in.At.UnixNano())
	effect := func(ctx context.Context, p router.Plan) (router.Effect, error) {
		actor := in.Actor
		if p.Routing.DecidedBy == trace.DecidedByJev {
			actor = trace.JevActor
		}
		var reason, id, path, content string
		if in.Redraft {
			pin, err := LatestPin(repo, stream)
			if err != nil {
				return router.Effect{}, err
			}
			encoded, err := shed.EncodeRedraft(shed.Redraft{Version: shed.Version, Round: n, Revision: pin, Note: note})
			if err != nil {
				return router.Effect{}, router.Refusal(err.Error())
			}
			reason = fmt.Sprintf("%s asked the architect for a redraft of %s after round %d: %s", proposer(actor), pin, n, note)
			id, path, content = shed.RedraftDocumentID(n), shed.RedraftPath(n), string(encoded)
		} else {
			encoded, err := shed.EncodeMore(shed.More{Version: shed.Version, Round: n, Rounds: 1})
			if err != nil {
				return router.Effect{}, router.Refusal(err.Error())
			}
			reason = fmt.Sprintf("%s asked for %s of debate after round %d: %s", proposer(actor), Rounds(1), n, note)
			id, path, content = shed.MoreDocumentID(n), shed.MorePath(n), string(encoded)
		}
		doc, err := ownerShedDocument(repo, p.Exit.Project, stream, id, path, content, p.At, actor)
		if err != nil {
			return router.Effect{}, err
		}
		return router.Effect{Documents: []trace.Document{doc}, Events: []trace.Event{trace.Notice(p.TransitionID, "owner", reason)}, Reason: reason}, nil
	}
	stay := router.Target{Name: TargetWait, Stay: true, NoMove: true,
		Description: "Leave the decision to the owner: the chief of staff does not ask for a redraft or another round by default, and the owner can ask for either."}
	asked := router.Target{Name: name, State: enters, Description: description, Bar: router.BarStrict, Works: true}
	x := router.Exit{
		Repo: repo, Project: o.Project, Workstream: stream, Machine: WorkstreamMachine,
		Subject: OwnerSubject, From: o.Owner.Value, Version: o.Owner.Version,
		Trigger: TriggerMoveProposal, Cause: cause, Actor: in.Actor, Note: note, Turn: in.Turn, At: in.At, Proposed: name,
		Default: TargetWait, Targets: []router.Target{stay, asked},
		Questions: []router.Question{ask("proposal_fits", "The proposed move fits the evidence."), ask("evidence_supports", "The history and the standing dissent support the move.")},
		TransitionID: func(t router.Target) string { return cause + "-" + t.Name },
		Enter: func(_ context.Context, p router.Plan) (router.Effect, error) {
			return router.Effect{Reason: fmt.Sprintf("workstream %s stays where it is: the proposal to ask for %s was not made: %s", stream, name, note)}, nil
		},
		Entering: map[string]router.EffectFunc{name: effect},
		Evidence: router.Evidence{
			Intent: "The committee debated the workstream's spec and plan and the debate concluded; the owner ratifies, asks for more rounds or asks for a redraft.",
			Facts:  map[string]any{"round": n, "standing_objections": len(shed.Standing(open)), "blocking_objections": len(shed.Blocked(open)), "target": name},
		},
		Prereq: router.Prerequisites{
			Other: func(_ context.Context, t router.Target) (string, error) {
				if t.Name == TargetWait {
					return "", nil
				}
				feature, err := repo.Workflow(stream, trace.FeatureSubject)
				if err != nil {
					return "", err
				}
				if feature.Value != InShedState {
					return fmt.Sprintf("workstream %s is %s; the debate runs while it is in the shed", stream, FeatureState(feature.Value)), nil
				}
				if skipped, err := SkippedDebate(repo, stream); err != nil || skipped {
					return "the owner skipped debate", err
				}
				return "", nil
			},
		},
	}
	for _, e := range open {
		x.Evidence.Records = append(x.Evidence.Records, router.Entry{Kind: "objection", ID: e.ID, Text: e.Argument})
	}
	return x, nil
}
