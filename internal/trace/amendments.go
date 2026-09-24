package trace

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// AmendmentRequest contains the checked citation and seal identity supplied
// by the tool. The trace checks lifecycle and turn identity under its lock.
type AmendmentRequest struct {
	QuestionID   string
	Citations    []string
	Change       string
	Reason       string
	Seal         int
	SealRevision int
	SpecHash     string
}

// FileAmendment atomically records a request, its notice and the requester's
// waiting transition. Repeated calls from the same turn return the same ID.
func (r *Repository) FileAmendment(ctx context.Context, agent string, scope coreadapter.Scope, req AmendmentRequest, at time.Time) (Amendment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if scope.Role != "mason" && scope.Role != "reviewer" && scope.Role != ChiefOfStaff {
		return Amendment{}, fmt.Errorf("only masons, reviewers and the chief of staff may file amendments")
	}
	turn, log, v, records, err := r.questionTurn(ctx, agent, scope, at)
	if err != nil {
		return Amendment{}, err
	}
	stream := config.WorkstreamID(scope.Workstream)
	for _, record := range records {
		if a, ok := record.(Amendment); ok && a.Workstream == stream && a.Actor.ID == agent && a.Thread == scope.Thread && a.Turn == scope.Turn {
			return a, nil
		}
	}
	if scope.Role != ChiefOfStaff {
		for _, q := range questions(records, v, stream) {
			if q.Asked.AskedBy.ID == agent && q.Asked.Thread == scope.Thread && q.Asked.Turn == scope.Turn {
				return Amendment{}, refused("this turn already asked question %s; end the turn", q.Asked.ID)
			}
		}
	}
	if state := v.states[FeatureSubject].Value; state != "building" && state != "assembled" {
		return Amendment{}, refused("amendments require a building or assembled workstream")
	}
	if len(req.Citations) == 0 || !present(req.Change) || !present(req.Reason) || req.Seal < 1 || req.SealRevision < 1 || !present(req.SpecHash) {
		return Amendment{}, refused("citations, proposed change, reason and sealed revision are required")
	}
	var latestSeal Document
	for _, record := range records {
		if d, ok := record.(Document); ok && d.Workstream == stream && d.Path == "seal.json" && d.Revision >= latestSeal.Revision {
			latestSeal = d
		}
	}
	if latestSeal.ID == "" || latestSeal.Revision != req.SealRevision {
		return Amendment{}, refused("the sealed revision changed; retry against the current spec and plan")
	}
	requester := Actor{Kind: "agent", ID: agent}
	unit := scope.Unit
	parkRole := scope.Role
	if scope.Role == ChiefOfStaff {
		if req.QuestionID == "" {
			return Amendment{}, refused("an open question is required")
		}
		q, err := openQuestion(questions(records, v, stream), req.QuestionID)
		if err != nil {
			return Amendment{}, err
		}
		requester, unit = q.Asked.AskedBy, q.Asked.Unit
		for _, record := range records {
			if identity, ok := record.(Agent); ok && identity.Workstream == stream && identity.ID == requester.ID {
				parkRole = identity.Role
			}
		}
	} else if req.QuestionID != "" {
		return Amendment{}, refused("only the chief of staff may route a question")
	}
	var from string
	if parkRole == "mason" || parkRole == "reviewer" {
		stage := map[string]string{"mason": "implementing", "reviewer": "reviewing"}[parkRole]
		from = v.states[UnitSubject(unit)].Value
		if unit == "" || from != stage && !(scope.Role == ChiefOfStaff && from == "waiting") {
			return Amendment{}, refused("the requesting unit must be %s or waiting on its question", stage)
		}
	}
	n := 1
	for _, record := range records {
		if a, ok := record.(Amendment); ok && a.Workstream == stream {
			if i, err := strconv.Atoi(a.ID); err == nil && i >= n {
				n = i + 1
			}
		}
	}
	id := strconv.Itoa(n)
	h := Header{Schema: "osmia.trace.amendment", Version: Version, ID: id, Revision: 1, Project: r.project, Workstream: stream, Unit: unit, At: at, Actor: Actor{Kind: "agent", ID: agent}, Cause: turn.Request.ID, Depth: turn.Request.Depth + 1}
	a := Amendment{Header: h, Requester: requester, Role: scope.Role, Thread: scope.Thread, Turn: scope.Turn, QuestionID: req.QuestionID, Citations: req.Citations, Change: req.Change, Reason: req.Reason, Seal: req.Seal, SealRevision: req.SealRevision, SpecHash: req.SpecHash}
	if err := validate(a); err != nil {
		return Amendment{}, err
	}
	var txs []Transaction
	if req.QuestionID != "" {
		q := questionTransition(v, h, req.QuestionID, QuestionOpen, QuestionRouted, fmt.Sprintf("The chief of staff routed question %s to amendment %s", req.QuestionID, id))
		txs = append(txs, q)
	}
	transitionID := "amendment_" + id + "_filed"
	transition := Transition{Header: Header{Schema: "osmia.trace.transition", Version: Version, ID: transitionID, Revision: 1, Project: r.project, Workstream: stream, Unit: unit, At: at, Actor: h.Actor, Cause: h.Cause, Depth: h.Depth}, Subject: "amendment_" + id, From: "", To: "filed", Reason: fmt.Sprintf("%s filed amendment %s", scope.Role, id)}
	txs = append(txs, Transaction{ExpectedVersion: 0, Transition: transition, Events: []Event{Notice(transitionID, "amendment", fmt.Sprintf("Amendment %s was filed by the %s for %s: %s", id, scope.Role, strings.Join(req.Citations, ", "), strings.TrimSpace(req.Reason)))}})
	if from != "" {
		subject := UnitSubject(unit)
		stage := map[string]string{"mason": "implementing", "reviewer": "reviewing"}[parkRole]
		wait := Transition{Header: Header{Schema: "osmia.trace.transition", Version: Version, ID: subject + "_waiting_amendment_" + id, Revision: 1, Project: r.project, Workstream: stream, Unit: unit, At: at, Actor: Actor{Kind: "service", ID: parkRole}, Cause: transitionID, Depth: h.Depth + 1}, Subject: subject, From: from, To: "waiting", Reason: fmt.Sprintf("unit %s waits for amendment %s; its %s stage and candidate are preserved", unit, id, stage)}
		txs = append(txs, Transaction{ExpectedVersion: v.states[subject].Version, Transition: wait})
	}
	files, _, err := r.stage(stream, log, v, []Record{a}, txs...)
	if err != nil {
		return Amendment{}, err
	}
	if err := r.publish(ctx, files); err != nil {
		return Amendment{}, err
	}
	_ = r.wake.Notify(context.Background())
	return a, nil
}

// OwnerTurn returns the request of the scope's active, uncaptured
// chief-of-staff turn and reports whether that turn answers a message from
// the owner.
func (r *Repository) OwnerTurn(agent string, scope coreadapter.Scope) (TurnRequest, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if scope.Role != ChiefOfStaff {
		return TurnRequest{}, false, fmt.Errorf("only a chief-of-staff turn answers the owner")
	}
	_, q, err := r.turnScope(agent, scope, true)
	if err != nil {
		return TurnRequest{}, false, err
	}
	return q.Request, q.Request.Actor.Kind == "owner", nil
}
