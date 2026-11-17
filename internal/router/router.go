package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

// ErrSchedulingPath is the error of routing an exit on the scheduling path,
// where Jev is never asked: a pass hands its exits to a Worker.
var ErrSchedulingPath = errors.New("an exit raised on the scheduling path is handed to the router's worker")

// Router routes exits. Its zero value is not usable: Judge is required, because
// Jev is always part of routing.
type Router struct {
	// Judge asks Jev the narrow questions.
	Judge *jev.Judge
	// Effects defines the side effects of entering each target.
	Effects *Effects
	// Rebuild defines how each machine rebuilds an exit from its route record,
	// so Recover can finish a pick that was recorded but not applied.
	Rebuild *Rebuilders
	// Spans receives the route span of each decided exit; nil sends none.
	Spans func(SpanRoute)
	Now   func() time.Time
	Log   *slog.Logger

	locks sync.Map
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Router) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Result is what routing one exit did.
type Result struct {
	// Outcome is followed, departed, held or fallback, as the route span says.
	Outcome string
	// Committed says the target was entered; State is the subject's state once
	// the exit is decided, so for a held exit the state it stays in.
	Committed bool
	State     string
	// Target is the state entered, or the move held back or sent to the owner.
	Target string
	// Journal is the state of the occurrence's route journal.
	Journal  string
	Hold     string
	Reason   string
	Judgment string
	Rule     string
	// Reused says the exit's occurrence was already decided and the recorded
	// result was returned without a request or a commit.
	Reused bool
	// Handed says the exit was handed to the worker, which decides it beside the
	// scheduling pass that raised it; nothing else in the result is known.
	Handed bool
}

func resultOf(rec Record, reused bool) Result {
	out := Result{Outcome: rec.Outcome, Journal: rec.State, Target: rec.Target, Hold: rec.Hold, Reason: rec.Reason, Judgment: rec.Judgment, Rule: rec.Rule, Reused: reused}
	out.Committed = rec.State == StateCommitted
	out.State = rec.From
	switch {
	case out.Committed && rec.Entered != "":
		out.State = rec.Entered
	case out.Committed:
		out.State = rec.Target
	}
	return out
}

// lock serialises the asks of one occurrence.
func (r *Router) lock(x Exit, key string) func() {
	id := fmt.Sprintf("%p/%s/%s", x.Repo, x.Workstream, key)
	m, _ := r.locks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// legality returns why the target is not legal for the exit now, or "". It is
// the one definition of the hard prerequisites: the router uses it to filter
// the targets Jev is offered and again, against current state, whenever it
// applies a target. A non-nil error is a failure to read.
func (r *Router) legality(ctx context.Context, x Exit, t Target) (string, error) {
	a := x.Authority
	switch {
	case t.Approval:
		// Approval comes only from a reviewer's verdict for the current
		// candidate or from the owner.
		if a == nil || a.Target != t.Name || a.Kind != AuthorityReviewer && a.Kind != AuthorityOwner {
			return "approval comes only from a reviewer verdict or the owner", nil
		}
	case t.Gate:
		// An owner gate is entered only on the owner's decision or one of Jev's
		// existing gate decisions.
		if a == nil || a.Target != t.Name || a.Kind != AuthorityOwner && a.Kind != AuthorityJevGate {
			return "an owner gate opens only on an owner decision or a gate decision of Jev", nil
		}
	}
	if a != nil && a.Target == t.Name && a.Valid != nil {
		if why, err := a.Valid(ctx); err != nil || why != "" {
			return why, err
		}
	}
	if t.NeedsCandidate {
		if x.Prereq.Candidate == nil {
			return "no candidate is known for the subject", nil
		}
		if why, err := x.Prereq.Candidate(ctx); err != nil || why != "" {
			return why, err
		}
	}
	for _, check := range []func(context.Context, Target) (string, error){x.Prereq.Operations, x.Prereq.Limits, x.Prereq.Other} {
		if check == nil {
			continue
		}
		if why, err := check(ctx, t); err != nil || why != "" {
			return why, err
		}
	}
	return "", nil
}

// legalTargets returns the targets of the exit that pass every hard
// prerequisite, in the exit's order, and why each of the others does not.
func (r *Router) legalTargets(ctx context.Context, x Exit) ([]Target, map[string]string, error) {
	var legal []Target
	why := map[string]string{}
	for _, t := range x.Targets {
		reason, err := r.legality(ctx, x, t)
		if err != nil {
			return nil, nil, err
		}
		if reason != "" {
			why[t.Name] = reason
			continue
		}
		legal = append(legal, t)
	}
	return legal, why, nil
}

// decision is a target chosen for an exit and how it was chosen.
type decision struct {
	target    string
	outcome   string
	decidedBy string
	judgment  string
	rule      string
	band      string
}

// Route decides one exit and commits the result, waiting in place for Jev up to
// its timeout. It must not run on the scheduling path: a scheduling pass hands
// its exits to a Worker. An exit whose occurrence was decided before, a
// restart in between included, returns the recorded result without asking Jev
// again; an occurrence whose pick was recorded but not applied is finished.
// The error is a failure to read or write; the exit is then routed again by
// the caller's next pass.
func (r *Router) Route(ctx context.Context, x Exit) (Result, error) {
	if r == nil || r.Judge == nil {
		return Result{}, errors.New("the router needs Jev")
	}
	if IsSchedulingPath(ctx) {
		return Result{}, ErrSchedulingPath
	}
	if err := x.validate(); err != nil {
		return Result{}, err
	}
	key := OccurrenceKey(x)
	name := journalName(key)
	defer r.lock(x, key)()

	rec, rev, journal, err := readRecord(x.Repo, x.Workstream, name)
	if err != nil {
		return Result{}, err
	}
	if rec != nil && rec.final() {
		return resultOf(*rec, true), nil
	}
	now := r.now()
	if rec == nil {
		raised := x.At
		if raised.IsZero() {
			raised = now
		}
		rec = &Record{Occurrence: key, Project: string(x.Project), Workstream: string(x.Workstream), Unit: x.Unit, Machine: x.Machine, Subject: x.Subject,
			Trigger: x.Trigger, Cause: x.Cause, From: x.From, Version: x.Version, Default: x.Default, Raised: raised,
			Actor: x.Actor, Note: x.Note, Turn: x.Turn, HeaderCause: x.HeaderCause}
		if x.Authority != nil {
			rec.Authority, rec.AuthorityRef = x.Authority.Kind, x.Authority.Ref
		}
	}
	state, err := x.Repo.RawWorkflow(x.Workstream, x.Subject)
	if err != nil {
		return Result{}, err
	}
	if state.Version != x.Version || state.Value != x.From {
		return r.stale(ctx, x, name, rec, rev, journal, state)
	}

	var d decision
	switch {
	case rec.State == StateDecided:
		d = decision{target: rec.Target, outcome: rec.Outcome, decidedBy: rec.DecidedBy, judgment: rec.Judgment, rule: rec.Rule, band: rec.Confidence}
	case x.Authority.pinned():
		d = decision{target: x.Authority.Target, outcome: OutcomeFollowed, decidedBy: x.Authority.Kind, judgment: x.Authority.Judgment, rule: "pinned", band: ConfidenceNone}
		rec.Table, rec.Rule, rec.Decided = TableVersion, d.rule, now
	default:
		var done *Result
		d, done, err = r.decide(ctx, x, name, rec, rev, journal)
		if err != nil || done != nil {
			if done == nil {
				return Result{}, err
			}
			return *done, err
		}
		// The pick is recorded before it is applied, so a restart finishes it.
		rec.State, rec.Table, rec.Rule, rec.Outcome, rec.Target, rec.DecidedBy, rec.Judgment, rec.Confidence, rec.Decided = StateDecided, TableVersion, d.rule, d.outcome, d.target, d.decidedBy, d.judgment, d.band, now
		w := journalWrite{rec: *rec, rev: rev, from: journal, to: StateDecided, reason: fmt.Sprintf("Route of %s decided: %s (table %s rule %s)", key, d.target, TableVersion, d.rule)}
		if err := r.writeJournal(ctx, x, name, w); err != nil {
			return Result{}, err
		}
		rev, journal.Version, journal.Value = rev+1, journal.Version+1, StateDecided
	}
	return r.apply(ctx, x, name, rec, rev, journal, d)
}

// decide asks the narrow questions of a non-pinned exit and returns the
// decision. When the exit is finished without one (held, sent to the owner),
// the result is returned instead.
func (r *Router) decide(ctx context.Context, x Exit, name string, rec *Record, rev int, journal trace.WorkflowState) (decision, *Result, error) {
	strict := r.Judge.DecisionThreshold()
	legal, why, err := r.legalTargets(ctx, x)
	if err != nil {
		return decision{}, nil, err
	}
	for _, t := range legal {
		rec.Offered = append(rec.Offered, t.Name)
	}
	rec.Table = TableVersion
	var def *Target
	for i := range legal {
		if legal[i].Name == x.Default {
			def = &legal[i]
		}
	}
	if def == nil {
		if t, ok := x.target(x.Default); ok && (t.Gate || t.Approval) && (x.Authority == nil || x.Authority.Target != t.Name) {
			// A proposal cannot make an owner gate: the move is the owner's.
			rec.Rule, rec.Target = "0:owner_gate", t.Name
			res, err := r.hold(ctx, x, name, rec, rev, journal, StateOwner, HoldGate, fmt.Sprintf("%s is an owner decision", t.Name), nil)
			return decision{}, &res, err
		}
		rec.Rule = "0:default_illegal"
		res, err := r.hold(ctx, x, name, rec, rev, journal, StateHeld, HoldDefaultIllegal, fmt.Sprintf("the default %s fails a hard prerequisite: %s", x.Default, why[x.Default]), nil)
		return decision{}, &res, err
	}
	// A default that is the approval a reviewer's verdict authorises is decided
	// by that verdict; any other default by code.
	byCode := trace.DecidedByCode
	if x.Authority != nil && x.Authority.Kind == AuthorityReviewer && x.Authority.Target == x.Default {
		byCode = trace.DecidedByReviewer
	}
	if len(legal) == 1 {
		// A choice needs two options: with one legal target the router keeps the
		// default itself and Jev is not asked.
		return decision{target: x.Default, outcome: OutcomeFollowed, decidedBy: byCode, rule: "0:only_target", band: ConfidenceNone}, nil, nil
	}

	options := make([]jev.TransitionOption, len(legal))
	for i, t := range legal {
		options[i] = jev.TransitionOption{Name: t.Name, Description: plain(t.Description, 500)}
	}
	own := make([]jev.TransitionQuestion, len(x.Questions))
	for i, q := range x.Questions {
		own[i] = jev.TransitionQuestion{Name: q.Name, Instructions: q.Instructions}
	}
	transitions, err := trace.Read[trace.Transition](x.Repo, x.Workstream)
	if err != nil {
		return decision{}, nil, err
	}
	s := buildSummary(x, legal, transitions, r.now())
	state, err := s.fit(summaryBudget(options))
	if err != nil {
		return decision{}, nil, err
	}
	sources := append([]jev.Source{}, x.Evidence.Sources...)
	req := jev.TransitionRequest{
		Scope: coreadapter.Scope{Project: string(x.Project), Workstream: string(x.Workstream), Unit: x.Unit},
		Cause: x.HeaderCause, Occurrence: rec.Occurrence, State: json.RawMessage(state), Options: options, Questions: own, Sources: sources,
	}
	rec.JevRequested = true
	res := r.Judge.DecideTransition(ctx, x.Repo, req)
	if ctx.Err() != nil && !res.Answered {
		return decision{}, nil, ctx.Err()
	}
	rec.Judgment = res.Judgment
	if !res.Answered {
		// Degraded mode: the default commits if it still passes the hard
		// prerequisites, which the apply step checks again.
		rec.Fallback = string(res.Reason)
		r.logger().Warn("Jev did not answer a transition request; the default is kept", "workstream", string(x.Workstream), "subject", x.Subject, "trigger", x.Trigger, "reason", string(res.Reason), "detail", res.Detail)
		return decision{target: x.Default, outcome: OutcomeFallback, decidedBy: trace.DecidedByFallback, judgment: res.Judgment, rule: "fallback:" + string(res.Reason), band: ConfidenceNone}, nil, nil
	}
	v := Decide(x.Default, legal, res.Answers, strict)
	rec.Rule = v.Rule
	view := describe(res.Answers, v)
	switch v.Action {
	case ActionHold:
		out, err := r.hold(ctx, x, name, rec, rev, journal, StateHeld, v.Hold, view, &v)
		return decision{}, &out, err
	case ActionOwner:
		out, err := r.hold(ctx, x, name, rec, rev, journal, StateOwner, HoldIntent, view, &v)
		return decision{}, &out, err
	}
	d := decision{target: v.Target, outcome: OutcomeFollowed, decidedBy: byCode, judgment: res.Judgment, rule: v.Rule, band: confidenceBand(v, true, strict)}
	if v.Departed {
		d.outcome, d.decidedBy = OutcomeDeparted, trace.DecidedByJev
	}
	rec.Reason = view
	return d, nil, nil
}
