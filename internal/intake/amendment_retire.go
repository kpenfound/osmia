package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// RetireStaleAmendment reconciles an undecided request against the sealed
// documents. A true result means this pass must not advance the request.
func (s *Controller) RetireStaleAmendment(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, id string) (bool, error) {
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return false, err
	}
	if feature.Value != BuildingState && feature.Value != AssembledState {
		return false, nil
	}
	c, found, err := ReadAmendment(repository, stream, id)
	if err != nil || !found {
		return false, err
	}
	switch c.State.Value {
	case "filed", "drafting", AmendmentWaiting, "proposed", "debating", "heard", "answering", "answered", AmendmentPresented:
	default:
		return false, nil
	}
	current, doc, sealed, err := seal.Latest(repository, stream)
	if err != nil || !sealed {
		return false, err
	}
	matching, err := AmendmentSealMatches(repository, stream, c.Request.SealRevision, current)
	if err != nil {
		// One unreadable filing seal must not prevent other amendments progressing.
		s.Log().Warn("cannot reconcile amendment filing seal", "workstream", stream, "amendment", id, "error", err)
		return true, nil
	}
	if matching {
		return false, nil
	}
	reason := fmt.Sprintf("the service retired amendment %s because the sealed spec or plan changed since its filing seal %d (seal.json revision %d); replacing seal %d (seal.json revision %d) is in force. File a new request against the current seal if the change is still needed", id, c.Request.Seal, c.Request.SealRevision, current.Seal, doc.Revision)
	record := AmendmentDecision{Actor: AmendmentActor.ID, Decision: AmendmentReject, Note: reason, Round: c.Round, Packet: c.Packet.Revision, Spec: c.Spec.Revision, Plan: c.Plan.Revision, SealRevision: doc.Revision}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return true, err
	}
	k := len(c.Decisions) + 1
	transition := AmendmentDecided(id, k)
	header := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: AmendmentDecisionID(id), Revision: k, Project: repository.Project(), Workstream: stream, Unit: c.Request.Unit, At: s.Now(), Actor: AmendmentActor, Cause: "amendment-retirement"}
	decision := trace.Document{Header: header, Path: AmendmentDecisionPath(id), Content: string(data) + "\n"}
	header.Schema, header.ID, header.Revision = "osmia.trace.transition", transition, 1
	tx := trace.Transaction{ExpectedVersion: c.State.Version, Transition: trace.Transition{Header: header, Subject: AmendmentSubject(id), From: c.State.Value, To: AmendmentRejected, Reason: reason}, Events: []trace.Event{trace.Notice(transition, "amendment", reason)}}
	_, err = repository.RecordDocumentsWith(ctx, []trace.Document{decision}, tx)
	if errors.Is(err, trace.ErrConflict) {
		return true, nil
	}
	return true, err
}

// AmendmentRetirementOutcome settles operations superseded by a service
// retirement, even when their own transition was never recorded.
func AmendmentRetirementOutcome(repository *trace.Repository, stream config.WorkstreamID, id string) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](repository, stream)
	if err != nil {
		return nil, err
	}
	for _, t := range transitions {
		if t.Subject == AmendmentSubject(id) && t.To == AmendmentRejected && t.Actor == AmendmentActor {
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

func (a AmendmentDebate) RetiredTurns(ctx context.Context, stream config.WorkstreamID, id string) error {
	result, err := AmendmentRetirementOutcome(a.Repository, stream, id)
	if err != nil || result == nil {
		return err
	}
	threads, err := a.Repository.Threads(stream)
	if err != nil {
		return err
	}
	asked, err := a.Repository.Questions(stream)
	if err != nil {
		return err
	}
	for _, th := range threads {
		if th.Identity.Role != ArchitectRole && th.Identity.Role != CommitteeRole {
			continue
		}
		chain := AskChain(th, "amend-"+id+"-", asked)
		var turns []string
		for _, q := range th.Turns {
			if _, ok := chain[q.Request.TurnID]; !ok && !strings.HasPrefix(q.Request.TurnID, "amend-"+id+"-") {
				continue
			}
			turns = append(turns, q.Request.TurnID)
			if q.CompletedAt.IsZero() && q.Response != nil && q.Claim != nil {
				current, err := a.Repository.CurrentTurnClaim(stream, q.Claim)
				if err != nil {
					return err
				}
				if current {
					continue
				}
				if err := a.Repository.CompleteTurn(ctx, stream, th.Identity.ID, q.Request.TurnID, q.Claim.Token, a.S.Now()); err != nil && !errors.Is(err, trace.ErrConflict) {
					return err
				}
			}
		}
		if _, err := a.Repository.CancelThreadTurns(ctx, stream, th.Identity.ID, turns, a.S.Now(), AmendmentActor, result.Evidence); err != nil {
			return err
		}
	}
	return nil
}

// settleRetiredAmendment reconciles retirement racing an operation's final
// write, so its superseded result cannot schedule another attempt.
func settleRetiredAmendment(repository *trace.Repository, stream config.WorkstreamID, id string, result *coreadapter.OperationResult, err *error) {
	retired, readErr := AmendmentRetirementOutcome(repository, stream, id)
	if readErr == nil && retired != nil {
		*result = *retired
		*err = nil
	}
}
