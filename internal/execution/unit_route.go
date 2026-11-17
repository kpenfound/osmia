package execution

import (
	"context"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// UnitMachine is the machine name of a unit's targets in the router's effects.
const UnitMachine = "unit"

// unitTargets describes the states a unit moves to, for the router. Checking,
// reviewing and approved work on the unit's candidate. Approved is entered only
// on a reviewer's verdict or the owner's decision.
func unitTargets() []router.Target {
	return []router.Target{
		{Name: UnitImplementing, Description: "Give the unit's mason a turn with a fresh clean-turn allowance to revise the unit.", Bar: router.BarModest},
		{Name: UnitChecking, Description: "Run the checks again on the unit's recorded candidate.", Bar: router.BarModest, NeedsCandidate: true},
		{Name: UnitReviewing, Description: "Have the unit's reviewer review the recorded candidate again.", Bar: router.BarModest, NeedsCandidate: true},
		{Name: UnitApproved, Description: "Approve the recorded candidate so the foreman lands it.", Approval: true, NeedsCandidate: true},
		{Name: UnitContested, Description: "Hold the unit for the owner to rule on.", Bar: router.BarModest},
	}
}

// UnitExitInput is what a caller knows of an exit of a started unit.
type UnitExitInput struct {
	Repo    *trace.Repository
	Stream  config.WorkstreamID
	Unit    string
	Trigger string
	Cause   string
	// Default is the state today's code would enter.
	Default string
	Note    string
	Turn    string
	Actor   trace.Actor
	// Authority is the authority the exit carries: the owner's decision, a
	// reviewer's verdict or one of Jev's gate decisions. A chief proposal and a
	// default carry none.
	Authority *router.Authority
	Evidence  router.Evidence
	Questions []router.Question
}

// UnitExit returns the exit of a started unit that has not merged, read at the
// state the unit is in now, with the targets a unit moves to, the hard
// prerequisites of the unit's targets, and the one definition of a landing or
// rebase in flight: a unit's targets check that unit's own operations. The
// transition into a target keeps the ID and the cause of a unit move, so the
// readers of moves recognise it.
func (s *Controller) UnitExit(in UnitExitInput) (router.Exit, error) {
	state, err := in.Repo.Workflow(in.Stream, trace.UnitSubject(in.Unit))
	if err != nil {
		return router.Exit{}, err
	}
	version := state.Version
	x := router.Exit{
		Repo: in.Repo, Project: in.Repo.Project(), Workstream: in.Stream, Unit: in.Unit, Machine: UnitMachine,
		Subject: trace.UnitSubject(in.Unit), From: state.Value, Version: version,
		Trigger: in.Trigger, Cause: in.Cause, HeaderCause: moveDocument(in.Unit, version), Actor: in.Actor, Note: in.Note, Turn: in.Turn,
		Default: in.Default, Targets: unitTargets(), Questions: in.Questions, Evidence: in.Evidence, Authority: in.Authority,
		TransitionID: func(t router.Target) string { return moveDocument(in.Unit, version) + "-" + t.Name },
		Prereq: router.Prerequisites{
			Candidate: func(context.Context) (string, error) {
				report, revision, err := unitReport(in.Repo, in.Stream, in.Unit)
				if err != nil {
					return "", err
				}
				if revision == 0 || report.Candidate == "" {
					return fmt.Sprintf("unit %s has no recorded candidate", in.Unit), nil
				}
				return "", nil
			},
			Operations: func(context.Context, router.Target) (string, error) {
				action, err := unitOperation(in.Repo, in.Stream, in.Unit)
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
	return x, nil
}

// RegisterUnitEffects defines, in the router's registry, the effect of entering
// each state a unit moves to. They are the records an owner's command and the
// chief of staff's tool make: the move with its note, a fresh clean-turn
// allowance for the mason, the candidate a fresh check run or review works on,
// the approval record the foreman lands, and the notice to the chief of staff.
// load returns the execution controller.
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
			cause := x.HeaderCause
			if cause == "" {
				cause = x.Cause
			}
			state := trace.WorkflowState{Version: x.Version, Value: x.From}
			rec, api := s.unitMoveRecords(ctx, x.Repo, x.Workstream, x.Unit, state, p.Target.Name, note, x.Actor, x.Turn, cause, p.TransitionID, p.At)
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

// RebuildUnitExit returns how the router rebuilds the exit of a started unit
// from its route record, to finish a pick that was recorded but not applied
// when the service stopped. An exit that carried an authority is rebuilt by the
// controller that raised it, which holds the verdict or the decision, so it is
// left for that controller's next pass. load returns the execution controller.
func RebuildUnitExit(load func() *Controller) router.Rebuilder {
	return func(_ context.Context, repo *trace.Repository, rec router.Record) (router.Exit, error) {
		s := load()
		if s == nil {
			return router.Exit{}, fmt.Errorf("unit moves are unavailable")
		}
		if rec.Authority != "" {
			return router.Exit{}, fmt.Errorf("the exit of %s carried the authority of %s", rec.Subject, rec.Authority)
		}
		return s.UnitExit(UnitExitInput{Repo: repo, Stream: config.WorkstreamID(rec.Workstream), Unit: rec.Unit, Trigger: rec.Trigger, Cause: rec.Cause,
			Default: rec.Default, Note: rec.Note, Turn: rec.Turn, Actor: rec.Actor})
	}
}
