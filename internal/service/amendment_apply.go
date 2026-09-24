package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// amendmentApplied is the state of an approved amendment once its effect on
// the workstream's units, approvals and final report is recorded.
const amendmentApplied = "applied"

// amendmentUnitID is the ID of the transition that reworks a unit for an
// amendment, or notifies it.
func amendmentUnitID(unit, id string, rework bool) string {
	if rework {
		return trace.UnitSubject(unit) + "_reworked_amendment_" + id
	}
	return trace.UnitSubject(unit) + "_notified_amendment_" + id
}

// amendmentMasonTurn is the mason turn that carries an amendment's notice and
// the unit's amended bundle.
func amendmentMasonTurn(unit, id string) string { return masonAgent(unit) + "-amendment-" + id }

// classifyAmendment returns the first revision of an amendment's application
// record: the revisions it moves the workstream from and to, the request and
// the owner's note, and the affected set the architect's draft recorded with
// its units classified against the sealed and approved plans.
func classifyAmendment(req trace.Amendment, d AmendmentDecision, affectedDoc, sealedPlan, approvedPlan trace.Document, from, to amendment.Pin) (amendment.Application, error) {
	var affected amendmentAffected
	if err := json.Unmarshal([]byte(affectedDoc.Content), &affected); err != nil {
		return amendment.Application{}, fmt.Errorf("%s revision %d does not parse: %v", affectedDoc.Path, affectedDoc.Revision, err)
	}
	before, err := plan.Parse([]byte(sealedPlan.Content))
	if err != nil {
		return amendment.Application{}, fmt.Errorf("the sealed plan does not parse: %v", err)
	}
	after, err := plan.Parse([]byte(approvedPlan.Content))
	if err != nil {
		return amendment.Application{}, fmt.Errorf("the approved plan does not parse: %v", err)
	}
	a := amendment.Application{Amendment: req.ID, From: from, To: to, Citations: req.Citations, Change: req.Change, Reason: req.Reason, Note: d.Note,
		Criteria: append([]string{}, affected.Criteria...), Proofs: append([]string{}, affected.Proofs...)}
	a.Rework, a.Notify, a.Added, a.Removed = amendment.Classify(a.Criteria, affected.Units, before, after)
	return a, nil
}

// application returns the latest revision of an amendment's application
// record, and that document; a document of revision zero when the amendment
// has none.
func (a amendmentDebate) application(stream config.WorkstreamID, id string) (amendment.Application, trace.Document, error) {
	docs, err := trace.Read[trace.Document](a.repository, stream)
	if err != nil {
		return amendment.Application{}, trace.Document{}, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == amendment.DocumentID(id) && d.Path == amendment.Path(id) {
			latest = d
		}
	}
	if latest.Revision == 0 {
		return amendment.Application{}, latest, nil
	}
	var out amendment.Application
	if err := json.Unmarshal([]byte(latest.Content), &out); err != nil {
		return amendment.Application{}, latest, fmt.Errorf("%s revision %d: %w", latest.Path, latest.Revision, err)
	}
	return out, latest, nil
}

func (a amendmentDebate) masons() *masons {
	return &masons{s: a.s, cfg: a.s.current(), repository: a.repository}
}

// apply records, in one commit, what a resealed amendment does to the
// workstream: each reworked unit that is implementing, reviewing or approved
// moves to implementing; each notified unit that is implementing stays
// there, and one that is reviewing or approved moves to reviewing; units the
// amended plan adds enter planned, or ready once every unit they depend on
// has merged; a changed criterion that a merged unit addresses becomes a
// follow-up unit, since merged units are never reopened; a final report that
// read the replaced revisions is named invalidated; and the amendment moves
// to applied. Waiting and contested units are held and moved once they leave
// that state. The second revision of amendments/<n>/application.json
// records the held units, follow-ups and invalidated final review. A unit
// whose state moved since it was read leaves the commit to the next pass.
// The mason turns the moves call for are queued after the commit.
func (a amendmentDebate) apply(ctx context.Context, stream config.WorkstreamID, req trace.Amendment, state trace.WorkflowState) error {
	app, doc, err := a.application(stream, req.ID)
	if err != nil {
		return err
	}
	if doc.Revision == 0 {
		return fmt.Errorf("resealed amendment %s has no %s", req.ID, amendment.Path(req.ID))
	}
	b, found, err := a.masons().read(stream)
	if err != nil || !found {
		return err
	}
	cause := "amendment-" + req.ID + "-" + amendmentResealed
	at := a.s.now()
	header := func(id, unit string) trace.Header {
		h := a.amendmentHeader(id, stream, unit, cause)
		h.At = at
		return h
	}
	var units []trace.Transaction
	held, merged := []string{}, []string{}
	var reworked, notified []string
	for _, group := range []struct {
		names  []string
		rework bool
	}{{app.Rework, true}, {app.Notify, false}} {
		for _, unit := range group.names {
			st := b.states[trace.UnitSubject(unit)]
			switch st.Value {
			case UnitWaiting, UnitContested:
				held = append(held, unit)
			case UnitMerged:
				if group.rework {
					merged = append(merged, unit)
				}
			case UnitImplementing, UnitReviewing, UnitApproved:
				units = append(units, a.amendUnit(stream, app, unit, st, group.rework, header))
				if group.rework {
					reworked = append(reworked, unit)
				} else {
					notified = append(notified, unit)
				}
			}
		}
	}
	var entered []string
	for _, id := range app.Added {
		u, ok := b.plan.Unit(id)
		subject := trace.UnitSubject(id)
		if !ok || b.states[subject].Value != "" {
			continue
		}
		planned := subject + "_planned_amendment_" + req.ID
		units = append(units, trace.Transaction{Transition: trace.Transition{Header: header(planned, id), Subject: subject, To: UnitPlanned,
			Reason: fmt.Sprintf("amendment %s adds unit %s to the plan of seal %d", req.ID, id, app.To.Seal)}})
		waiting := slices.DeleteFunc(slices.Clone(u.DependsOn), func(d string) bool { return b.states[trace.UnitSubject(d)].Value == UnitMerged })
		if len(waiting) > 0 {
			entered = append(entered, fmt.Sprintf("%s planned (waiting for %s)", id, strings.Join(waiting, ", ")))
			continue
		}
		units = append(units, trace.Transaction{ExpectedVersion: 1, Transition: trace.Transition{Header: header(subject+"_ready_amendment_"+req.ID, id), Subject: subject, From: UnitPlanned, To: UnitReady,
			Reason: fmt.Sprintf("unit %s is ready: every unit it depends on has merged", id)}})
		entered = append(entered, id+" ready")
	}
	docs := []trace.Document{}
	added, err := a.followups(stream, app, b.plan, merged)
	if err != nil {
		return err
	}
	var followups []string
	if len(added) > 0 {
		content, err := json.MarshalIndent(added, "", "  ")
		if err != nil {
			return err
		}
		rev, err := nextRevision(a.repository, stream, followup.DocumentID)
		if err != nil {
			return err
		}
		docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: followup.DocumentID, Revision: rev, Project: a.repository.Project(), Workstream: stream, At: at, Actor: amendmentActor, Cause: cause},
			Path: followup.Path, Content: string(content) + "\n"})
		for _, f := range added {
			followups = append(followups, f.Unit.ID)
			subject := trace.UnitSubject(f.Unit.ID)
			units = append(units,
				trace.Transaction{Transition: trace.Transition{Header: header(subject+"-"+UnitPlanned, f.Unit.ID), Subject: subject, To: UnitPlanned, Reason: f.Gap}},
				trace.Transaction{ExpectedVersion: 1, Transition: trace.Transition{Header: header(subject+"-"+UnitReady, f.Unit.ID), Subject: subject, From: UnitPlanned, To: UnitReady,
					Reason: fmt.Sprintf("follow-up %s is ready to address %s after amendment %s", f.Unit.ID, f.Criterion, req.ID)}})
		}
	}
	applied := amendment.Applied{Held: held, Followups: append([]string{}, followups...)}
	id := "amendment-" + req.ID + "-" + amendmentApplied
	events := []trace.Event{}
	report, reported, err := latestFinalReport(a.repository, stream)
	if err != nil {
		return err
	}
	if reported && report.Seal == app.From.Seal && report.Spec == app.From.Spec && report.Plan == app.From.Plan {
		applied.FinalReview = report.Review
		body := fmt.Sprintf("Final review %d read seal %d, spec revision %d and plan revision %d, which amendment %s replaced: its report no longer authorises delivery", report.Review, report.Seal, report.Spec, report.Plan, req.ID)
		if _, _, approval, err := deliveryDocuments(a.repository, stream); err != nil {
			return err
		} else if approval != nil && approval.Review == report.Review {
			body += ", and the owner's delivery approval of it no longer holds"
		}
		events = append(events, trace.Notice(id, "final-review", body+". Final review runs again once every unit has merged."))
	}
	app.Applied = &applied
	content, err := json.MarshalIndent(app, "", "  ")
	if err != nil {
		return err
	}
	docs = append(docs, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: doc.ID, Revision: doc.Revision + 1, Project: a.repository.Project(), Workstream: stream, At: at, Actor: amendmentActor, Cause: cause},
		Path: doc.Path, Content: string(content) + "\n"})
	reason := fmt.Sprintf("approved amendment %s is applied to seal %d: reworked %s; notified %s; held until they leave waiting or contested %s; added %s; follow-ups %s", req.ID, app.To.Seal, list(reworked), list(notified), list(held), list(entered), list(followups))
	if applied.FinalReview > 0 {
		reason += fmt.Sprintf("; final review %d is invalidated", applied.FinalReview)
	}
	events = append([]trace.Event{trace.Notice(id, "amendment", reason)}, events...)
	txs := append([]trace.Transaction{{ExpectedVersion: state.Version, Transition: trace.Transition{Header: header(id, req.Unit), Subject: amendmentSubject(req.ID), From: amendmentResealed, To: amendmentApplied, Reason: reason}, Events: events}}, units...)
	if _, err := a.repository.RecordDocumentsWith(ctx, docs, txs...); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return nil
		}
		return err
	}
	if err := a.s.step("amendment-applied"); err != nil {
		return err
	}
	return a.follow(ctx, stream, req)
}

// amendUnit returns the transaction that reworks or notifies one unit in the
// state it is in: a reworked unit moves to implementing; a notified unit
// stays implementing, or moves to reviewing from reviewing or approved.
func (a amendmentDebate) amendUnit(stream config.WorkstreamID, app amendment.Application, unit string, st trace.WorkflowState, rework bool, header func(id, unit string) trace.Header) trace.Transaction {
	id := amendmentUnitID(unit, app.Amendment, rework)
	to, reason := UnitImplementing, ""
	switch {
	case rework:
		changed := app.Criteria
		if u, err := sealedUnit(a.repository, coreadapter.Scope{Workstream: string(stream), Unit: unit}); err == nil {
			if addressed := slices.DeleteFunc(landingCriteria(u), func(c string) bool { return !slices.Contains(app.Criteria, c) }); len(addressed) > 0 {
				changed = addressed
			}
		}
		reason = fmt.Sprintf("unit %s returns to implementing from %s: amendment %s changed the meaning of %s, which it addresses; its mason builds it again against seal %d, spec revision %d and plan revision %d", unit, st.Value, app.Amendment, list(changed), app.To.Seal, app.To.Spec, app.To.Plan)
	case st.Value == UnitImplementing:
		reason = fmt.Sprintf("unit %s stays implementing: amendment %s changed its plan entry, and the criteria it addresses keep their meaning; its mason's next turn carries the notice and the amended bundle", unit, app.Amendment)
	default:
		to = UnitReviewing
		reason = fmt.Sprintf("unit %s returns to review from %s: amendment %s changed its plan entry, and the criteria it addresses keep their meaning; its review is taken again against plan revision %d with the notice", unit, st.Value, app.Amendment, app.To.Plan)
	}
	return trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{Header: header(id, unit), Subject: trace.UnitSubject(unit), From: st.Value, To: to, Reason: reason},
		Events: []trace.Event{trace.Notice(id, "unit", "Unit "+unit+": "+reason+".")}}
}

// followups returns one follow-up unit for every changed criterion that a
// merged unit addresses, scoped to the footprints of the plan's units that
// address it, unless it is recorded already.
func (a amendmentDebate) followups(stream config.WorkstreamID, app amendment.Application, p plan.Plan, merged []string) ([]followup.Unit, error) {
	previous, err := followup.Read(a.repository, stream)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, u := range p.Units {
		known[u.ID] = true
	}
	for _, u := range previous {
		known[u.Unit.ID] = true
	}
	var out []followup.Unit
	for _, criterion := range app.Criteria {
		var by []string
		var proof plan.Proof
		for _, id := range merged {
			u, ok := p.Unit(id)
			if !ok {
				continue
			}
			for _, address := range u.Addresses {
				if address.Criterion == criterion {
					by = append(by, id)
					if proof.Kind == "" {
						proof = address.Proof
					}
				}
			}
		}
		id := fmt.Sprintf("amendment-%s-%s", app.Amendment, strings.ReplaceAll(criterion, "#", "-"))
		if len(by) == 0 || known[id] {
			continue
		}
		var footprint []string
		for _, u := range p.Units {
			if slices.ContainsFunc(u.Addresses, func(address plan.Address) bool { return address.Criterion == criterion }) {
				for _, name := range u.Footprint {
					if !slices.Contains(footprint, name) {
						footprint = append(footprint, name)
					}
				}
			}
		}
		gap := fmt.Sprintf("amendment %s changed the meaning of %s after %s merged; bring the merged work in line with the amended criterion", app.Amendment, criterion, strings.Join(by, ", "))
		out = append(out, followup.Unit{Amendment: app.Amendment, Criterion: criterion, Gap: gap,
			Unit: plan.Unit{ID: id, Title: "Address an amended criterion", Addresses: []plan.Address{{Criterion: criterion, Proof: proof}}, DependsOn: []string{}, Footprint: footprint}})
	}
	return out, nil
}

// follow finishes an applied amendment's unit moves on every pass: a held
// unit that left waiting or contested is reworked or notified in the state
// it is in now, and every move to implementing gets the mason turn that
// carries the notice and the amended bundle. Each step finds what an earlier
// pass did, so a restart between them completes the rest once.
func (a amendmentDebate) follow(ctx context.Context, stream config.WorkstreamID, req trace.Amendment) error {
	app, _, err := a.application(stream, req.ID)
	if err != nil || app.Applied == nil {
		return err
	}
	transitions, err := trace.Read[trace.Transition](a.repository, stream)
	if err != nil {
		return err
	}
	recorded := func(id string) (trace.Transition, bool) {
		i := slices.IndexFunc(transitions, func(t trace.Transition) bool { return t.ID == id })
		if i < 0 {
			return trace.Transition{}, false
		}
		return transitions[i], true
	}
	cause := "amendment-" + req.ID + "-" + amendmentApplied
	for _, unit := range app.Applied.Held {
		rework := slices.Contains(app.Rework, unit)
		if _, ok := recorded(amendmentUnitID(unit, req.ID, rework)); ok {
			continue
		}
		st, err := a.repository.Workflow(stream, trace.UnitSubject(unit))
		if err != nil {
			return err
		}
		if st.Value != UnitImplementing && st.Value != UnitReviewing && st.Value != UnitApproved {
			continue
		}
		tx := a.amendUnit(stream, app, unit, st, rework, func(id, unit string) trace.Header { return a.amendmentHeader(id, stream, unit, cause) })
		if _, err := a.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
			return err
		}
	}
	if len(app.Applied.Held) > 0 {
		if transitions, err = trace.Read[trace.Transition](a.repository, stream); err != nil {
			return err
		}
	}
	for _, unit := range append(slices.Clone(app.Rework), app.Notify...) {
		moved, ok := recorded(amendmentUnitID(unit, req.ID, slices.Contains(app.Rework, unit)))
		if !ok || moved.To != UnitImplementing {
			continue
		}
		if err := a.enqueueAmended(ctx, stream, app, unit, moved); err != nil {
			return err
		}
	}
	return nil
}

// enqueueAmended queues the mason turn that tells a unit's mason about the
// amendment and carries the unit's amended bundle, unless the mason's thread
// holds it already. The thread is created when the unit has none. A unit
// whose bundle cannot be assembled because its spec no longer matches its
// seal gets no turn, and the reason is recorded as the unit's block.
func (a amendmentDebate) enqueueAmended(ctx context.Context, stream config.WorkstreamID, app amendment.Application, unit string, moved trace.Transition) error {
	agent, turn := masonAgent(unit), amendmentMasonTurn(unit, app.Amendment)
	m := a.masons()
	th, err := a.repository.Thread(stream, agent)
	switch {
	case errors.Is(err, os.ErrNotExist):
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: agent, Revision: 1, Project: a.repository.Project(), Workstream: stream, Unit: unit, At: moved.At, Actor: masonActor, Cause: moved.ID}, Role: masonRole, ThreadID: agent}
		if err := a.repository.CreateThread(ctx, identity); err != nil {
			return err
		}
	case err != nil:
		return err
	case slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn }):
		return nil
	}
	mason, err := m.bundle(ctx, stream, unit)
	if errors.Is(err, bundle.ErrStaleSpec) {
		return m.block(ctx, stream, unit, moved.ID, fmt.Sprintf("unit %s is implementing after amendment %s, and its mason's amended bundle cannot be assembled: %v", unit, app.Amendment, err))
	}
	if err != nil {
		return err
	}
	profile, _, err := a.s.roleExecution(m.cfg, masonRole)
	if err != nil {
		return err
	}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: a.repository.Project(), Workstream: stream, Unit: unit, At: a.s.now(), Actor: amendmentActor, Cause: moved.ID, Depth: 1},
		AgentID: agent, ThreadID: agent, TurnID: turn, Profile: profile, SystemPrompt: masonSystemPrompt(m.cfg.Project), Prompt: amendedMasonPrompt(app, mason, slices.Contains(app.Rework, unit))}
	_, err = a.repository.EnqueueTurn(ctx, req)
	return err
}

// amendedMasonPrompt is the text of the mason turn that follows an approved
// amendment: what it means for the unit, then the unit's amended bundle with
// the amendment's notice.
func amendedMasonPrompt(app amendment.Application, m bundle.Mason, rework bool) string {
	what := fmt.Sprintf("The owner approved amendment %s. It changed the meaning of criteria unit %s addresses, so the unit returns to implementing: build it again in your existing workspace against the amended spec and plan below, and put in place and pass the proofs they name.", app.Amendment, m.Unit)
	if !rework {
		what = fmt.Sprintf("The owner approved amendment %s. It changed the entry of unit %s in the plan; the criteria the unit addresses keep their meaning. Continue in your existing workspace against the amended plan below, and stay within its footprint.", app.Amendment, m.Unit)
	}
	return fmt.Sprintf("%s When every criterion of the unit holds and its proof is in place and passing, call done with the outcome of your work and a report on every criterion of the unit, then end your turn.\n\n%s", what, m.Render())
}
