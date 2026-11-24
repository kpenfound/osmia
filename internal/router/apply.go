package router

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/trace"
)

func (r *Router) apply(ctx context.Context, x Exit, name string, rec *Record, rev int, journal trace.WorkflowState, d decision) (Result, error) {
	rec.Target = d.target
	target, ok := x.target(d.target)
	if !ok {
		return r.refuse(ctx, x, name, rec, rev, journal, fmt.Sprintf("the target %s is not among the exit's targets", d.target))
	}
	if held, err := r.paused(ctx, x, target); err != nil {
		return Result{}, err
	} else if held != "" {
		return deferred(x, target.Name, held), nil
	}
	why, err := r.legality(ctx, x, target)
	if err != nil {
		return Result{}, err
	}
	if why != "" {
		return r.refuse(ctx, x, name, rec, rev, journal, fmt.Sprintf("%s is no longer legal: %s", target.Name, why))
	}
	at := r.now()
	moveID := journalSubject(name) + "-move"
	if x.TransitionID != nil {
		moveID = x.TransitionID(target)
	}
	routing := trace.Routing{DecidedBy: d.decidedBy, DefaultTo: x.defaultState()}
	if d.decidedBy != trace.DecidedByFallback && d.decidedBy != trace.DecidedByOwner && d.decidedBy != trace.DecidedByChief {
		routing.Judgment = d.judgment
	}
	reason := target.Reason
	if reason == "" {
		reason = fmt.Sprintf("%s: %s", x.Trigger, target.Description)
	}
	plan := Plan{Exit: x, Target: target, TransitionID: moveID, Reason: reason, At: at, Routing: routing}
	effect, err := r.Effects.build(ctx, plan)
	switch {
	case errors.Is(err, ErrCannotCarryOut):
		return r.refuse(ctx, x, name, rec, rev, journal, err.Error())
	case err != nil:
		return Result{}, err
	}
	if effect.Reason != "" {
		reason = effect.Reason
	}
	if d.outcome != OutcomeFollowed {
		reason = fmt.Sprintf("%s [decide-transition table %s, rule %s, %s]", reason, TableVersion, d.rule, d.decidedBy)
	}

	cause := x.Cause
	if x.HeaderCause != "" {
		cause = x.HeaderCause
	}
	if effect.Cause != "" {
		cause = effect.Cause
	}
	if cause == "" {
		cause = x.Trigger
	}
	h := r.headerFor(x, "osmia.trace.transition", moveID, 1, at, cause)
	h.Actor = x.Actor
	entered := target.entered()
	if !trace.ValidRouting(entered, routing) {
		return r.refuse(ctx, x, name, rec, rev, journal, fmt.Sprintf("routing %s cannot be recorded for %s", routing.DecidedBy, entered))
	}
	rec.State, rec.Target, rec.Outcome, rec.DecidedBy, rec.Judgment, rec.Rule, rec.Confidence, rec.Decided = StateCommitted, target.Name, d.outcome, d.decidedBy, d.judgment, d.rule, d.band, at
	rec.Entered, rec.Transition = x.From, ""
	txs := append([]trace.Transaction{}, effect.With...)
	if !target.NoMove {
		move := trace.Transaction{ExpectedVersion: x.Version, Events: effect.Events, Transition: trace.Transition{
			Header: h, Subject: x.Subject, From: x.From, To: entered, Reason: reason,
			DecidedBy: routing.DecidedBy, Judgment: routing.Judgment, DefaultTo: routing.DefaultTo}}
		txs = append([]trace.Transaction{move}, txs...)
		rec.Entered, rec.Transition = entered, moveID
	}
	if rec.Table == "" {
		rec.Table = TableVersion
	}
	write := journalWrite{rec: *rec, rev: rev, from: journal, to: StateCommitted,
		reason: fmt.Sprintf("Route of %s committed %s (table %s rule %s)", rec.Occurrence, target.Name, rec.Table, d.rule),
		docs:   effect.Documents, txs: txs}
	if target.NoMove {
		write.events = effect.Events
	}
	if err := r.writeJournal(ctx, x, name, write); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if again, _, _, rerr := readRecord(x.Repo, x.Workstream, name); rerr == nil && again != nil && again.final() {
				return resultOf(*again, true), nil
			}
			state, serr := x.Repo.RawWorkflow(x.Workstream, x.Subject)
			if serr == nil && (state.Version != x.Version || state.Value != x.From) {
				rec.State, rec.Transition = StateDecided, ""
				return r.stale(ctx, x, name, rec, rev, journal, state)
			}
		}
		return Result{}, err
	}
	r.span(x, *rec, target.Name)
	return resultOf(*rec, false), nil
}

func (r *Router) refuse(ctx context.Context, x Exit, name string, rec *Record, rev int, journal trace.WorkflowState, why string) (Result, error) {
	if rec.JevRequested && rec.Judgment != "" && rec.Fallback == "" {
		if err := r.Judge.Refuse(ctx, x.Repo, x.Workstream, rec.Judgment, why); err != nil {
			r.logger().Warn("cannot record the refusal of a transition pick", "workstream", string(x.Workstream), "judgment", rec.Judgment, "error", err.Error())
		}
	}
	rec.State, rec.Outcome, rec.Refusal, rec.Hold, rec.Decided = StateRefused, OutcomeHeld, why, HoldRefused, r.now()
	body := fmt.Sprintf("The move of %s from %s to %s (trigger %s) could not be applied: %s. Nothing was committed and none of its effects ran. The subject is held at %s.", x.Subject, x.From, rec.Target, x.Trigger, why, x.From)
	return r.finishHeld(ctx, x, name, rec, rev, journal, body)
}

func (r *Router) hold(ctx context.Context, x Exit, name string, rec *Record, rev int, journal trace.WorkflowState, state, why, view string, v *Verdict) (Result, error) {
	rec.State, rec.Outcome, rec.Hold, rec.Decided = state, OutcomeHeld, why, r.now()
	if v != nil {
		rec.Target = v.Target
		rec.Confidence = confidenceBand(*v, true, r.Judge.DecisionThreshold())
	}
	rec.Reason = view
	rec.Table = TableVersion
	var body string
	switch why {
	case HoldIntent:
		body = fmt.Sprintf("Jev would not move %s from %s to %s on its own (trigger %s): acting would change what the owner approved. The move is the owner's to approve. Jev's view: %s.", x.Subject, x.From, rec.Target, x.Trigger, view)
	case HoldGate:
		where := "the recovery card"
		if x.OwnerCard != "" {
			where = x.OwnerCard
		}
		body = fmt.Sprintf("The move of %s from %s to %s (trigger %s) is an owner decision, and a proposal cannot make it. Nothing was committed. The owner decides it on %s.", x.Subject, x.From, rec.Target, x.Trigger, where)
	case HoldOrdersDisagree:
		body = fmt.Sprintf("Jev held %s at %s without moving it (trigger %s): its two orderings of the targets disagreed. Jev's view: %s. Nothing was committed.", x.Subject, x.From, x.Trigger, view)
	case HoldBelowBar:
		body = fmt.Sprintf("Jev held %s at %s without moving it (trigger %s): the best target %s is below its bar while the default %s does not fit. Jev's view: %s. Nothing was committed.", x.Subject, x.From, x.Trigger, rec.Target, x.Default, view)
	default:
		body = fmt.Sprintf("%s was held at %s without moving it (trigger %s): %s. Nothing was committed.", x.Subject, x.From, x.Trigger, view)
	}
	return r.finishHeld(ctx, x, name, rec, rev, journal, body)
}

func (r *Router) finishHeld(ctx context.Context, x Exit, name string, rec *Record, rev int, journal trace.WorkflowState, body string) (Result, error) {
	id := journalSubject(name) + "-" + rec.State
	w := journalWrite{rec: *rec, rev: rev, from: journal, to: rec.State, reason: fmt.Sprintf("Route of %s %s: %s", rec.Occurrence, rec.State, rec.Hold),
		events: []trace.Event{trace.Notice(id, "chief", body)}}
	if err := r.writeJournal(ctx, x, name, w); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if again, _, _, rerr := readRecord(x.Repo, x.Workstream, name); rerr == nil && again != nil && again.final() {
				return resultOf(*again, true), nil
			}
		}
		return Result{}, err
	}
	r.span(x, *rec, x.From)
	return resultOf(*rec, false), nil
}

func (r *Router) stale(ctx context.Context, x Exit, name string, rec *Record, rev int, journal trace.WorkflowState, now trace.WorkflowState) (Result, error) {
	rec.State, rec.Outcome, rec.Hold, rec.Decided = StateStale, OutcomeHeld, HoldStale, r.now()
	rec.Reason = fmt.Sprintf("the subject is %s at version %d; the exit was raised at %s, version %d", now.Value, now.Version, x.From, x.Version)
	w := journalWrite{rec: *rec, rev: rev, from: journal, to: StateStale, reason: fmt.Sprintf("Route of %s is stale: %s", rec.Occurrence, rec.Reason)}
	if err := r.writeJournal(ctx, x, name, w); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if again, _, _, rerr := readRecord(x.Repo, x.Workstream, name); rerr == nil && again != nil && again.final() {
				return resultOf(*again, true), nil
			}
		}
		return Result{}, err
	}
	r.span(x, *rec, now.Value)
	return resultOf(*rec, false), nil
}

func (r *Router) span(x Exit, rec Record, chosen string) {
	if r.Spans == nil {
		return
	}
	band := rec.Confidence
	if band == "" {
		band = ConfidenceNone
	}
	r.Spans(SpanRoute{
		Project: x.Project, Workstream: x.Workstream, Unit: x.Unit, Occurrence: rec.Occurrence, Trigger: x.Trigger,
		Default: x.Default, Chosen: chosen, Outcome: rec.Outcome, Confidence: band, Judgment: rec.Judgment,
		Start: rec.Raised, End: rec.Decided,
	})
}
