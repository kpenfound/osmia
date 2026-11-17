package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// UnitMachine is the machine name of a unit's targets in the router's effects.
const UnitMachine = "unit"

// Router is the transition router as the unit controllers use it. A caller that
// waits on a request routes its exit in place; a scheduling pass hands its exit
// to the router's worker and goes on, so Jev is never asked on the scheduling
// path.
type Router interface {
	Route(ctx context.Context, x router.Exit) (router.Result, error)
	Submit(x router.Exit, done func(router.Result, error)) bool
}

// The triggers of a unit's exits. Each is a trigger of the route span.
const (
	TriggerMasonDone         = "unit.mason_done"
	TriggerMasonEndedClean   = "unit.mason_ended_clean"
	TriggerMarkersLeft       = "unit.markers_left"
	TriggerTurnFailed        = "unit.turn_failed"
	TriggerChecksPassed      = "unit.checks_passed"
	TriggerChecksFailed      = "unit.checks_failed"
	TriggerChecksIncomplete  = "unit.checks_incomplete"
	TriggerReviewApproved    = "unit.review_approved"
	TriggerReviewRejected    = "unit.review_rejected"
	TriggerReviewNoVerdict   = "unit.review_no_verdict"
	TriggerReviewRefused     = "unit.review_refused"
	TriggerReviewNeedsChecks = "unit.review_needs_checks"
	TriggerQuestionWait      = "unit.question_wait_broken"
	TriggerQuestionFollow    = "unit.question_follow"
	TriggerRebased           = "unit.rebased"
	TriggerRebaseConflicted  = "unit.rebase_conflicted"
	TriggerLanded            = "unit.landed"
	TriggerLandingRefused    = "unit.landing_refused"
	TriggerMoveOwner         = "unit.move_owner"
	TriggerMoveProposal      = "unit.move_proposal"
	TriggerStalled           = "unit.stalled"
	TriggerOwnerRuling       = "contest.owner_ruling"
	TriggerJevRuling         = "contest.jev_ruling"
	TriggerChiefRuling       = "contest.chief_ruling"
)

// TargetWait is the target of an exit that leaves the unit where it is: nothing
// moves while the exit's records and notices are committed. It is the default of
// a stalled unit.
const TargetWait = "wait"

// StalledTargets returns the targets of a stalled unit in the given state: wait,
// which is the default, and, for a unit that has started, a fresh turn of its
// mason or a fresh run of the stage it waits in. A unit that has not started
// has nothing to start again.
func StalledTargets(state string) []string {
	switch state {
	case UnitImplementing, UnitWaiting, UnitChecking, UnitReviewing, UnitApproved, UnitContested:
		return []string{TargetWait, UnitImplementing, UnitChecking, UnitReviewing}
	}
	return []string{TargetWait}
}

// unitTarget describes the states a unit moves to, for the router. Checking,
// reviewing and approved work on the unit's candidate. Approved is entered only
// on a reviewer's verdict or the owner's decision, and merged only after a
// landing.
func unitTarget(name string) router.Target {
	switch name {
	case TargetWait:
		return router.Target{Name: name, Description: "Leave the unit where it is and keep waiting; nothing moves.", Stay: true, NoMove: true}
	case UnitImplementing:
		return router.Target{Name: name, Description: "Give the unit's mason a turn with a fresh clean-turn allowance to revise the unit.", Bar: router.BarModest, Works: true}
	case UnitChecking:
		return router.Target{Name: name, Description: "Run the checks again on the unit's recorded candidate.", Bar: router.BarModest, NeedsCandidate: true, Works: true}
	case UnitReviewing:
		return router.Target{Name: name, Description: "Have the unit's reviewer review the recorded candidate again.", Bar: router.BarModest, NeedsCandidate: true, Works: true}
	case UnitApproved:
		return router.Target{Name: name, Description: "Approve the recorded candidate so the foreman lands it.", Approval: true, NeedsCandidate: true}
	case UnitContested:
		return router.Target{Name: name, Description: "Hold the unit for the owner to rule on.", Bar: router.BarModest}
	case UnitMerged:
		return router.Target{Name: name, Description: "Record the unit as merged: the foreman landed its approved candidate on the feature branch.", NeedsCandidate: true}
	case UnitReady:
		return router.Target{Name: name, Description: "Make the unit ready: every unit it depends on has merged."}
	case UnitWaiting:
		return router.Target{Name: name, Description: "Park the unit until the question its worker asked is answered; its workspace is kept."}
	}
	return router.Target{Name: name, Description: name}
}

// standardTargets are the states the owner and the chief of staff move a
// started unit to.
var standardTargets = []string{UnitImplementing, UnitChecking, UnitReviewing, UnitApproved, UnitContested}

// UnitExitInput is what a caller knows of an exit of a started unit.
type UnitExitInput struct {
	Repo    *trace.Repository
	Stream  config.WorkstreamID
	Unit    string
	Trigger string
	// Cause identifies the record that raised the exit, so that exits that
	// leave the state unchanged are different occurrences.
	Cause string
	// State is the unit's state the caller read and decided on. The zero value
	// reads the unit's current state.
	State trace.WorkflowState
	// Default is the state today's code would enter.
	Default string
	// Targets are the states the trigger can enter, the default among them;
	// empty means the states an owner moves a unit to. The router offers Jev
	// those that pass the hard prerequisites.
	Targets []string
	Note    string
	Turn    string
	Actor   trace.Actor
	// Authority is the authority the exit carries: the owner's decision, a
	// reviewer's verdict or one of Jev's gate decisions. A chief proposal and a
	// default carry none.
	Authority *router.Authority
	Evidence  router.Evidence
	Questions []router.Question
	// How the default target is entered: the transition's ID, the record it
	// cites, its reason and its notices. Empty means the shared effect of the
	// target, as for a move.
	ID, HeaderCause, Reason string
	Events                  []trace.Event
	// Documents are the trigger's own records, written whichever target is
	// entered.
	Documents []trace.Document
	// With are other subjects' transactions recorded in the same commit as the
	// default target.
	With []trace.Transaction
	// Candidate, when set, says whether the exit's candidate exists, instead of
	// the unit's recorded report: the report an exit records is its candidate.
	Candidate func(context.Context) (string, error)
	// Active says the workstream must be building or assembled, as it must
	// for a move.
	Active bool
	// Operation is the landing or rebase operation raising the exit; its own
	// in-flight state does not hold the exit.
	Operation string
}

// UnitExit returns the exit of a started unit that has not merged, with the
// targets the trigger can enter, the hard prerequisites of the unit's targets,
// and the one definition of a landing or rebase in flight: a unit's targets
// check that unit's own operations. A default is entered with the records it
// has always had; any other target, however chosen, is entered through the
// shared effects of a unit move, whose transition keeps the ID and the cause of
// a move so the readers of moves recognise it.
func (s *Controller) UnitExit(in UnitExitInput) (router.Exit, error) {
	state := in.State
	if state.Version == 0 && state.Value == "" {
		var err error
		if state, err = in.Repo.Workflow(in.Stream, trace.UnitSubject(in.Unit)); err != nil {
			return router.Exit{}, err
		}
	}
	version := state.Version
	names := in.Targets
	if len(names) == 0 {
		names = standardTargets
	}
	spec := triggerSpecs[in.Trigger]
	targets := make([]router.Target, 0, len(names))
	for _, name := range names {
		t := unitTarget(name)
		if d, ok := spec.describe[name]; ok {
			t.Description = d
		}
		t.Corroborate = spec.corroborate[name]
		targets = append(targets, t)
	}
	questions := in.Questions
	if len(questions) == 0 {
		questions = spec.questions
	}
	move := moveDocument(in.Unit, version)
	x := router.Exit{
		Repo: in.Repo, Project: in.Repo.Project(), Workstream: in.Stream, Unit: in.Unit, Machine: UnitMachine,
		Subject: trace.UnitSubject(in.Unit), From: state.Value, Version: version,
		Trigger: in.Trigger, Cause: in.Cause, HeaderCause: move, Actor: in.Actor, Note: in.Note, Turn: in.Turn,
		Default: in.Default, Targets: targets, Questions: questions, Evidence: in.Evidence, Authority: in.Authority,
		TransitionID: func(t router.Target) string {
			if in.ID != "" && t.Name == in.Default {
				return in.ID
			}
			return move + "-" + t.Name
		},
		Prereq: router.Prerequisites{
			Candidate: func(ctx context.Context) (string, error) {
				if in.Candidate != nil {
					return in.Candidate(ctx)
				}
				report, revision, err := unitReport(in.Repo, in.Stream, in.Unit)
				if err != nil {
					return "", err
				}
				if revision == 0 || report.Candidate == "" {
					return fmt.Sprintf("unit %s has no recorded candidate", in.Unit), nil
				}
				return "", nil
			},
			Operations: func(_ context.Context, t router.Target) (string, error) {
				// The state a trigger enters as its default is entered whatever
				// the unit's operations are doing, as it always has been; any
				// other target waits for the landing or rebase to end.
				if in.ID != "" && t.Name == in.Default {
					return "", nil
				}
				action, err := unitOperationExcept(in.Repo, in.Stream, in.Unit, in.Operation)
				switch {
				case err != nil:
					return "", err
				case action == LandAction:
					return fmt.Sprintf("the foreman is landing unit %s", in.Unit), nil
				case action == RebaseAction:
					return fmt.Sprintf("the foreman is rebasing unit %s", in.Unit), nil
				}
				return "", nil
			},
			Other: func(_ context.Context, t router.Target) (string, error) {
				if !in.Active {
					return "", nil
				}
				feature, err := in.Repo.Workflow(in.Stream, trace.FeatureSubject)
				if err != nil {
					return "", err
				}
				if feature.Value != BuildingState && feature.Value != AssembledState {
					return fmt.Sprintf("the workstream is %s; units move while it is building or assembled", featureState(feature.Value)), nil
				}
				return "", nil
			},
		},
	}
	if in.ID != "" {
		x.Enter = func(_ context.Context, p router.Plan) (router.Effect, error) {
			return router.Effect{Events: in.Events, Reason: in.Reason, Cause: in.HeaderCause, With: in.With}, nil
		}
	}
	if len(in.Documents) != 0 {
		x.Records = func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{Documents: in.Documents}, nil
		}
	}
	return x, nil
}

// raise routes the exit of a started unit. A scheduling pass sets scheduled: when
// the scheduler runs it, its exit is handed to the router's worker, which asks
// Jev beside the pass, and the caller treats the unit as handled until a later
// pass finds it moved or held. Any other caller waits in place, a pass run
// directly included. The result of a handed-off exit is empty.
func (s *Controller) raise(ctx context.Context, scheduled bool, in UnitExitInput) (router.Result, error) {
	if s.Router == nil {
		return router.Result{}, errors.New("routing unit exits is unavailable")
	}
	if in.Evidence.Intent == "" {
		in.Evidence.Intent = s.unitIntent(in.Repo, in.Stream, in.Unit)
	}
	x, err := s.UnitExit(in)
	if err != nil {
		return router.Result{}, err
	}
	if scheduled && router.IsSchedulingPath(ctx) {
		s.Router.Submit(x, nil)
		return router.Result{Handed: true}, nil
	}
	return s.Router.Route(ctx, x)
}

// sentBack reports whether a routed exit sent the unit to its mason by the
// trigger's own default, whose findings the caller queues, and not by another
// target, whose move gives the mason its turn.
func sentBack(res router.Result) bool {
	return res.Committed && res.Target == UnitImplementing && res.Outcome != router.OutcomeDeparted
}

// unitIntent is the sealed task and acceptance of the unit, as the intent Jev
// reads; a unit the seal does not name has none.
func (s *Controller) unitIntent(repo *trace.Repository, stream config.WorkstreamID, unit string) string {
	u, err := sealedUnit(repo, coreadapter.Scope{Workstream: string(stream), Unit: unit})
	if err != nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Task of unit %s: %s", unit, u.Task)
	for _, a := range u.Acceptance {
		fmt.Fprintf(&b, "\nAcceptance: %s", a)
	}
	return b.String()
}

// unitOperationExcept is unitOperation for an operation other than the one
// named: the operation that raises an exit does not hold it.
func unitOperationExcept(repo *trace.Repository, stream config.WorkstreamID, unit, except string) (string, error) {
	records, err := repo.Operations(stream)
	if err != nil {
		return "", err
	}
	for _, r := range records {
		if r.Result != nil || r.Operation.ID == except || r.Operation.Action != LandAction && r.Operation.Action != RebaseAction {
			continue
		}
		if in, err := operationUnit(r.Operation); err != nil {
			return "", err
		} else if in == unit {
			return r.Operation.Action, nil
		}
	}
	return "", nil
}

// RegisterUnitEffects defines, in the router's registry, the effect of entering
// each state a unit moves to. They are the records an owner's command and the
// chief of staff's tool make, and every other way a unit is moved to a state
// its trigger does not enter as a default: the move with its note, a fresh
// clean-turn allowance for the mason, the candidate a fresh check run or review
// works on, the approval record the foreman lands, and the notice to the chief
// of staff. load returns the execution controller.
func RegisterUnitEffects(reg *router.Effects, load func() *Controller) {
	for _, target := range moveTargets {
		reg.Register(UnitMachine, target, func(ctx context.Context, p router.Plan) (router.Effect, error) {
			s := load()
			if s == nil {
				return router.Effect{}, fmt.Errorf("unit moves are unavailable")
			}
			x := p.Exit
			note := x.Note
			if note == "" {
				note = p.Reason
			}
			cause, err := s.moveCause(x)
			if err != nil {
				return router.Effect{}, err
			}
			actor := x.Actor
			if p.Routing.DecidedBy == trace.DecidedByJev {
				actor = trace.JevActor
			}
			state := trace.WorkflowState{Version: x.Version, Value: x.From}
			rec, api := s.unitMoveRecords(ctx, x.Repo, x.Workstream, x.Unit, state, p.Target.Name, note, actor, x.Turn, cause, p.TransitionID, p.At)
			switch {
			case api == nil:
				return router.Effect{Documents: rec.docs, Events: rec.tx.Events, Reason: rec.tx.Transition.Reason}, nil
			case api.Code == Conflict || api.Code == Validation:
				return router.Effect{}, router.Refusal(api.Message)
			}
			return router.Effect{}, fmt.Errorf("%s", api.Message)
		})
	}
}

// moveCause is the record a move cites: the chief-of-staff turn that made it, or
// else the transition into the state it leaves; for any other exit, the record
// that raised it.
func (s *Controller) moveCause(x router.Exit) (string, error) {
	if x.Turn != "" {
		return x.Turn, nil
	}
	switch x.Trigger {
	case TriggerMoveOwner, TriggerMoveProposal, TriggerOwnerRuling, TriggerJevRuling, TriggerChiefRuling:
		transitions, err := trace.Read[trace.Transition](x.Repo, x.Workstream)
		if err != nil {
			return "", err
		}
		cause := ""
		for _, t := range transitions {
			if t.Subject == x.Subject {
				cause = t.ID
			}
		}
		return cause, nil
	}
	return x.Cause, nil
}

// RebuildUnitExit returns how the router rebuilds the exit of a started unit
// from its route record, to finish a pick that was recorded but not applied
// when the service stopped. Only a move proposal is rebuilt from its record: an
// exit that carried an authority, the records of its trigger or its own way of
// entering the default is rebuilt by the controller that raised it, which holds
// them, and is left for that controller's next pass. load returns the execution
// controller.
func RebuildUnitExit(load func() *Controller) router.Rebuilder {
	return func(_ context.Context, repo *trace.Repository, rec router.Record) (router.Exit, error) {
		s := load()
		if s == nil {
			return router.Exit{}, fmt.Errorf("unit moves are unavailable")
		}
		if rec.Authority != "" || rec.Trigger != TriggerMoveProposal {
			return router.Exit{}, fmt.Errorf("the exit of %s carried more than its record holds", rec.Subject)
		}
		return s.UnitExit(UnitExitInput{Repo: repo, Stream: config.WorkstreamID(rec.Workstream), Unit: rec.Unit, Trigger: rec.Trigger, Cause: rec.Cause,
			Default: rec.Default, Note: rec.Note, Turn: rec.Turn, Actor: rec.Actor, Active: true})
	}
}

// triggerSpec is what is particular to a trigger: the one-line description of
// a target as this trigger offers it, the trigger's own narrow questions, and
// what their answers must show for the router to depart to a target.
type triggerSpec struct {
	describe    map[string]string
	questions   []router.Question
	corroborate map[string][]router.Condition
}

func question(name, instructions string) router.Question {
	return router.Question{Name: name, Instructions: instructions}
}

func atLeast(q string, l router.Level) router.Condition { return router.Condition{Question: q, AtLeast: l} }
func atMost(q string, l router.Level) router.Condition  { return router.Condition{Question: q, AtMost: l} }

// triggerSpecs defines, per trigger of a unit's exits, what the router asks Jev
// beyond the questions every exit has. Each question is answered as a
// probability of a proposition about the records in the summary.
var triggerSpecs = map[string]triggerSpec{
	TriggerMasonDone: {
		describe:  map[string]string{UnitChecking: "Run the project's checks on the candidate the mason reported.", UnitContested: "Hold the unit for the owner: its report says it cannot meet its acceptance."},
		questions: []router.Question{question("report_blocked", "The mason's report says its work is blocked or the acceptance cannot hold."), question("report_matches", "The report's outcome addresses the unit's task.")},
		corroborate: map[string][]router.Condition{UnitContested: {atLeast("report_blocked", router.LevelModest)}},
	},
	TriggerMasonEndedClean: endedCleanSpec,
	TriggerMarkersLeft:     endedCleanSpec,
	TriggerTurnFailed: {
		describe: map[string]string{UnitImplementing: "Queue a fresh turn for the unit's mason.", UnitReviewing: "Run a new review turn over the same candidate.", UnitContested: "Hold the unit for the owner to rule on."},
		questions: []router.Question{question("transient", "The failure looks like infrastructure and not like the behaviour of the work."), question("repeats", "The same failure has repeated across the turns shown.")},
		corroborate: map[string][]router.Condition{UnitImplementing: {atLeast("transient", router.LevelModest), atMost("repeats", router.LevelHalf)}, UnitReviewing: {atLeast("transient", router.LevelModest), atMost("repeats", router.LevelHalf)}},
	},
	TriggerChecksPassed: {
		describe:    map[string]string{UnitReviewing: "Start the independent review of the candidate.", UnitChecking: "Run the checks again: the passing run does not cover what the change can affect."},
		questions:   []router.Question{question("pass_credible", "The passing run covered what the change can affect.")},
		corroborate: map[string][]router.Condition{UnitChecking: {atMost("pass_credible", router.LevelLow)}},
	},
	TriggerChecksFailed: {
		describe:  map[string]string{UnitImplementing: "Send the unit back to its mason with the failed checks.", UnitChecking: "Run the checks again: the failure looks environmental.", UnitContested: "Hold the unit for the owner to rule on."},
		questions: []router.Question{question("environmental", "The failure is infrastructure or flaky and not caused by the change."), question("actionable", "The check output tells the mason what to fix."), question("progress", "Successive send-backs got closer to passing.")},
		corroborate: map[string][]router.Condition{UnitChecking: {atLeast("environmental", router.LevelModest)}, UnitImplementing: {atLeast("actionable", router.LevelModest), atLeast("progress", router.LevelModest)}},
	},
	TriggerChecksIncomplete: {
		describe:    map[string]string{UnitContested: "Hold the unit for the owner to rule on.", UnitChecking: "Run the checks again."},
		questions:   []router.Question{question("environmental", "The failure to complete is infrastructure."), question("persistent", "The same error repeats across the runs shown.")},
		corroborate: map[string][]router.Condition{UnitChecking: {atLeast("environmental", router.LevelModest), atMost("persistent", router.LevelHalf)}},
	},
	TriggerReviewApproved: {
		describe:  map[string]string{UnitApproved: "Approve the candidate on the reviewer's satisfactory verdict so the foreman lands it.", UnitReviewing: "Have the candidate reviewed again.", UnitChecking: "Run the checks again before the candidate is approved.", UnitContested: "Hold the unit for the owner to rule on."},
		questions: []router.Question{question("verdict_consistent", "The reviewer's summary shows every acceptance item verified with no unresolved material point."), question("checks_cover", "The check evidence covers the changed behaviour.")},
		corroborate: map[string][]router.Condition{UnitReviewing: {atMost("verdict_consistent", router.LevelLow)}, UnitChecking: {atMost("checks_cover", router.LevelLow)}},
	},
	TriggerReviewRejected: {
		describe:  map[string]string{UnitImplementing: "Send the unit back to its mason with the reviewer's findings.", UnitReviewing: "Have the candidate reviewed again: the findings look mistaken.", UnitContested: "Hold the unit for the owner to rule on."},
		questions: []router.Question{question("findings_actionable", "The findings tell the mason what to change."), question("findings_mistaken", "The findings contradict the acceptance or are out of scope."), question("progress", "Successive send-backs got closer to approval.")},
		corroborate: map[string][]router.Condition{UnitReviewing: {atLeast("findings_mistaken", router.LevelModest)}, UnitImplementing: {atLeast("findings_actionable", router.LevelModest), atLeast("progress", router.LevelModest)}},
	},
	TriggerReviewNoVerdict: {
		describe:    map[string]string{UnitContested: "Hold the unit for the owner to rule on.", UnitReviewing: "Run a new review turn over the same candidate."},
		questions:   []router.Question{question("transient", "The missing verdict looks like a one-off.")},
		corroborate: map[string][]router.Condition{UnitReviewing: {atLeast("transient", router.LevelModest)}},
	},
	TriggerReviewRefused: {
		describe:    map[string]string{UnitReviewing: "Review the candidate again.", UnitChecking: "Run the checks again: the candidate changed.", UnitContested: "Hold the unit for the owner to rule on."},
		questions:   []router.Question{question("candidate_changed", "The inputs changed, so a fresh check run is right.")},
		corroborate: map[string][]router.Condition{UnitChecking: {atLeast("candidate_changed", router.LevelModest)}},
	},
	TriggerLandingRefused: {
		describe:  map[string]string{UnitChecking: "Run the checks again: the approval no longer holds.", UnitReviewing: "Have the candidate reviewed again.", UnitImplementing: "Send the unit back to its mason.", UnitContested: "Hold the unit for the owner to rule on.", UnitApproved: "Land again: the approval still holds and the refusal was not about the candidate."},
		questions: []router.Question{question("input_stale", "The landing was refused because an input of the approval is stale."), question("environmental", "The landing failed for a reason of infrastructure.")},
		corroborate: map[string][]router.Condition{UnitApproved: {atLeast("environmental", router.LevelModest), atMost("input_stale", router.LevelHalf)}},
	},
	TriggerStalled: {
		describe: map[string]string{
			TargetWait:       "Keep waiting: the records show the work this state waits for is still going to happen.",
			UnitImplementing: "Give the unit's mason a fresh turn: nothing shows its work is still going on.",
			UnitChecking:     "Run the checks again on the recorded candidate: the run this state waits for never ended.",
			UnitReviewing:    "Have the candidate reviewed again: the review this state waits for never ended.",
		},
		questions: []router.Question{question("work_lost", "Nothing in the records shows the work this state waits for is still going to happen."), question("stage_repeatable", "Starting the stage again repeats it without losing recorded work.")},
		corroborate: map[string][]router.Condition{
			UnitImplementing: {atLeast("work_lost", router.LevelModest), atLeast("stage_repeatable", router.LevelModest)},
			UnitChecking:     {atLeast("work_lost", router.LevelModest), atLeast("stage_repeatable", router.LevelModest)},
			UnitReviewing:    {atLeast("work_lost", router.LevelModest), atLeast("stage_repeatable", router.LevelModest)},
		},
	},
	TriggerMoveProposal: {
		questions: []router.Question{question("proposal_fits", "The proposed move fits the evidence."), question("evidence_supports", "The history and rulings support the move.")},
	},
}

var endedCleanSpec = triggerSpec{
	describe:    map[string]string{UnitContested: "Hold the unit for the owner to rule on.", UnitImplementing: "Give the unit's mason another turn with a fresh clean-turn allowance."},
	questions:   []router.Question{question("progress", "Recent turns changed the work."), question("needs_human", "The mason is blocked on a decision only a human gives.")},
	corroborate: map[string][]router.Condition{UnitImplementing: {atLeast("progress", router.LevelModest), atMost("needs_human", router.LevelHalf)}},
}
