package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// The machines of the exits this package raises, as the router's effects know
// them: a dependency wait (a cross-repo edge wait or a same-repo base wait) and
// a drift rebase.
const (
	DependencyMachine = "dep"
	DriftMachine      = "drift"
)

// The triggers of the exits a dependency wait and a drift rebase raise. Each is
// a trigger of the route span.
const (
	TriggerEdgeResolved       = "dep.edge_resolved"
	TriggerBaseAvailable      = "dep.base_available"
	TriggerBaseIntegrated     = "dep.base_integrated"
	TriggerDriftHeld          = "drift.held"
	TriggerDriftReviewBounces = "drift.review_rejected"
	TriggerDriftTurnFailed    = "drift.turn_failed"
)

// The states a dependency wait's exits enter.
const (
	// DependencyAvailable ends the wait: the dependent goes on.
	DependencyAvailable = "available"
	// DependencyWaiting keeps the dependent waiting.
	DependencyWaiting = "waiting"
)

// The targets of a held drift rebase's exit.
const (
	// DriftHoldTarget holds the drift rebase for the owner.
	DriftHoldTarget = "held"
	// DriftRetryTarget asks for the drift rebase again from the start.
	DriftRetryTarget = "retried"
)

// DriftRetried names the transition that asks for the drift rebase again after
// a held one; it ends the operation of the drift rebase that was held, as
// holding it does.
const DriftRetried = "retried"

// routeExit routes an exit. A scheduling pass hands it to the router's worker,
// which asks Jev beside the pass, and the pass treats the subject as handled
// until a later pass finds it moved. Any other caller waits in place.
func (s *Controller) routeExit(ctx context.Context, x router.Exit) (router.Result, error) {
	if s.Router == nil {
		return router.Result{}, errors.New("routing dependency and drift exits is unavailable")
	}
	if router.IsSchedulingPath(ctx) {
		s.Router.Submit(x, nil)
		return router.Result{Handed: true}, nil
	}
	return s.Router.Route(ctx, x)
}

// waitNotice is the kind of notice a dependency wait's transition carries.
func waitNotice(subject string) string {
	if subject == EdgeWaitSubject {
		return "edge"
	}
	return "base"
}

func question(name, instructions string) router.Question {
	return router.Question{Name: name, Instructions: instructions}
}

// RegisterRoutingEffects defines, in the router's registry, the effect of
// entering each state a dependency wait or a drift rebase moves to by an exit's
// target: the notice of a wait kept, and the request of a drift rebase asked
// for again. load returns the lifecycle controller.
func RegisterRoutingEffects(reg *router.Effects, load func() *Controller) {
	reg.Register(DependencyMachine, DependencyWaiting, func(_ context.Context, p router.Plan) (router.Effect, error) {
		return router.Effect{Events: []trace.Event{trace.Notice(p.TransitionID, waitNotice(p.Exit.Subject), p.Reason)}, Cause: p.Exit.Cause}, nil
	})
	reg.Register(DependencyMachine, DependencyAvailable, func(_ context.Context, p router.Plan) (router.Effect, error) {
		return router.Effect{Events: []trace.Event{trace.Notice(p.TransitionID, waitNotice(p.Exit.Subject), p.Reason)}}, nil
	})
	reg.Register(DriftMachine, DriftRetryTarget, func(ctx context.Context, p router.Plan) (router.Effect, error) {
		s := load()
		if s == nil {
			return router.Effect{}, errors.New("drift rebases are unavailable")
		}
		x := p.Exit
		state, err := x.Repo.Workflow(x.Workstream, DriftSubject)
		if err != nil {
			return router.Effect{}, err
		}
		if state.Version != x.Version || state.Value != x.From {
			return router.Effect{}, router.Refusal("the drift rebase moved since the exit was raised")
		}
		k, err := DriftNumber(state.Value)
		if err != nil {
			return router.Effect{}, router.Refusal(err.Error())
		}
		k++
		_, event := DriftIDs(k)
		input, err := json.Marshal(DriftInput{Drift: k})
		if err != nil {
			return router.Effect{}, err
		}
		op := coreadapter.Operation{ID: trace.OperationID(x.Repo.Project(), x.Workstream, event), Boundary: coreadapter.RepositoryBoundary, Action: DriftAction, Input: input}
		cfg := s.About(x.Repo)
		reason := fmt.Sprintf("%s; drift rebase %d fetches %s of %s and rebases %s onto it again, with fresh turns of its mason and reviewer", p.Reason, k, cfg.Project.BaseBranch, cfg.Project.Upstream, featureBranch(x.Workstream))
		return router.Effect{Reason: reason, Events: []trace.Event{{ID: event, Kind: DriftAction, Body: fmt.Sprintf("Rebase %s onto upstream", featureBranch(x.Workstream)), Operation: &op}}}, nil
	})
}

// clearing is what the exit from a dependency wait that has cleared is built
// from.
type clearing struct {
	Subject, Trigger string
	// Notice is the kind of the notice the transition carries.
	Notice string
	// Cause identifies the observation the wait cleared on, so a wait Jev kept
	// is asked about once for it; HeaderCause is the cause the cleared wait's
	// transition has always cited.
	Cause, HeaderCause string
	Reason             string
	// Doubt is the question the router asks to depart to keeping the wait.
	Doubt   router.Question
	Facts   map[string]any
	Records []router.Entry
	// Intent says what the wait protects.
	Intent string
	// Unresolved returns why the wait does not clear now, or "" while it does.
	Unresolved func(ctx context.Context) (string, error)
}

// clearingExit returns the exit from the wait of state: its default ends the
// wait, entered as the wait pass always entered it; keeping the dependent
// waiting is the one other target. The wait is derived from records the pass
// reads again, so the exit is raised again for the same observation only until
// Jev has kept the wait for it.
func (s *Controller) clearingExit(repo *trace.Repository, stream config.WorkstreamID, state trace.WorkflowState, c clearing) router.Exit {
	id := fmt.Sprintf("%s-%d", c.Subject, state.Version+1)
	return router.Exit{
		Repo: repo, Project: repo.Project(), Workstream: stream, Machine: DependencyMachine,
		Subject: c.Subject, From: state.Value, Version: state.Version, Trigger: c.Trigger, Cause: c.Cause, HeaderCause: c.HeaderCause, Actor: foremanActor,
		At:      s.Now(),
		Default: DependencyAvailable,
		Targets: []router.Target{
			{Name: DependencyAvailable, Description: "End the wait: what the dependent waited on is resolved, so its work is scheduled again.", Reason: c.Reason},
			{Name: DependencyWaiting, Stay: true, Bar: router.BarModest, Description: "Keep the dependent waiting: the records show its dependency is not yet usable, whatever the observation says.",
				Reason:      "Jev kept the dependent waiting although its dependency looks resolved",
				Corroborate: []router.Condition{{Question: c.Doubt.Name, AtLeast: router.LevelModest}}},
		},
		Questions:    []router.Question{c.Doubt},
		Evidence:     router.Evidence{Intent: c.Intent, Facts: c.Facts, Records: c.Records},
		TransitionID: func(router.Target) string { return id },
		Enter: func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{Events: []trace.Event{trace.Notice(id, c.Notice, c.Reason)}, Reason: c.Reason}, nil
		},
		Prereq: router.Prerequisites{Other: func(ctx context.Context, t router.Target) (string, error) {
			if t.Name != DependencyAvailable || c.Unresolved == nil {
				return "", nil
			}
			return c.Unresolved(ctx)
		}},
	}
}

// keptWaiting reports whether Jev already kept the wait of subject for the
// observation cause names: its latest transition is a departure to waiting
// that cites it.
func keptWaiting(repo *trace.Repository, stream config.WorkstreamID, subject, cause string) (bool, error) {
	transitions, err := trace.Read[trace.Transition](repo, stream)
	if err != nil {
		return false, err
	}
	var last trace.Transition
	for _, t := range transitions {
		if t.Subject == subject {
			last = t
		}
	}
	return last.Departed() && last.To == DependencyWaiting && last.Cause == cause, nil
}

// codeDefault marks a transition the router did not choose between targets for
// as the default its machine takes.
func codeDefault(t trace.Transition) trace.Transition {
	t.DecidedBy, t.DefaultTo = trace.DecidedByCode, t.To
	return t
}

// heldExit returns the exit that holds a drift rebase whose conflict
// resolution stopped. Holding it for the owner is the default; the other
// target asks for the drift rebase again from the start. The hold is entered
// as the drift always held it.
func (d Drifter) heldExit(stream config.WorkstreamID, state trace.WorkflowState, rebase DriftRebase, operation string, held DriftHeldError) router.Exit {
	transition, _ := DriftIDs(rebase.Drift)
	trigger := held.Trigger
	if trigger == "" {
		trigger = TriggerDriftHeld
	}
	reason := fmt.Sprintf("drift rebase %d is held for the owner: %s; feature branch %s stays at %s, the seal is unchanged, and no drift rebase is asked for until the owner asks for one or the chief of staff hands this one back with a note for its drift mason and reviewer", rebase.Drift, held.Why, rebase.Branch, rebase.Before)
	if rebase.Verdict != nil && rebase.Verdict.Summary != "" {
		reason += "; the last review found: " + rebase.Verdict.Summary
	}
	visible := fmt.Sprintf("Drift rebase %d onto upstream %s is held for the owner: %s. The feature branch and the seal stay, and no drift rebase is asked for until the owner asks for one or the chief of staff hands this one back with a note for its drift mason and reviewer.", rebase.Drift, rebase.Upstream.Commit, held.Why)
	id := transition + "-" + DriftHeld
	retry := router.Target{Name: DriftRetryTarget, State: fmt.Sprintf("requested-%d", rebase.Drift+1), Bar: router.BarModest,
		Description: "Ask for the drift rebase again from the start: the conflicts are replayed and fresh turns of the drift mason and reviewer resolve them.",
		Reason:      fmt.Sprintf("Jev asked for drift rebase %d again instead of holding drift rebase %d: %s", rebase.Drift+1, rebase.Drift, held.Why)}
	retry.Corroborate = held.corroborate()
	facts := map[string]any{"drift": rebase.Drift, "review_rounds": rebase.Review, "max_bounces": d.cfg.Shed.MaxBounces, "conflicted_paths": len(rebase.Conflicts)}
	records := []router.Entry{{Kind: "drift-hold", Text: held.Why}}
	if rebase.Verdict != nil && rebase.Verdict.Summary != "" {
		records = append(records, router.Entry{Kind: "drift-review", ID: rebase.Operation, Text: rebase.Verdict.Summary})
	}
	if len(rebase.Conflicts) > 0 {
		records = append(records, router.Entry{Kind: "drift-conflicts", Text: strings.Join(rebase.Conflicts, ", ")})
	}
	return router.Exit{
		Repo: d.repository, Project: d.repository.Project(), Workstream: stream, Machine: DriftMachine,
		Subject: DriftSubject, From: state.Value, Version: state.Version, Trigger: trigger, Cause: fmt.Sprintf("%s-held", transition), HeaderCause: operation, Actor: foremanActor,
		At:      d.s.Now(),
		Default: DriftHoldTarget,
		Targets: []router.Target{
			{Name: DriftHoldTarget, State: fmt.Sprintf("%s-%d", DriftHeld, rebase.Drift), Reason: reason,
				Description: "Hold the drift rebase for the owner: the feature branch and the seal stay, and no drift rebase is asked for until the owner asks or the chief of staff hands it back."},
			retry,
		},
		Questions: held.questions(),
		Evidence:  router.Evidence{Intent: "Keep the workstream's feature branch current with upstream without losing its work.", Facts: facts, Records: records},
		TransitionID: func(t router.Target) string {
			if t.Name == DriftRetryTarget {
				return transition + "-" + DriftRetried
			}
			return id
		},
		Enter: func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{Events: []trace.Event{trace.Notice(id, "drift", visible)}, Reason: reason}, nil
		},
	}
}

// corroborate says what the answers must show for the router to ask for the
// drift rebase again.
func (e DriftHeldError) corroborate() []router.Condition {
	switch e.Trigger {
	case TriggerDriftReviewBounces:
		return []router.Condition{{Question: "actionable", AtLeast: router.LevelModest}, {Question: "progress", AtLeast: router.LevelModest}}
	}
	return []router.Condition{{Question: "transient", AtLeast: router.LevelModest}, {Question: "repeats", AtMost: router.LevelHalf}}
}

// questions are the trigger's own narrow questions of a held drift rebase.
func (e DriftHeldError) questions() []router.Question {
	switch e.Trigger {
	case TriggerDriftReviewBounces:
		return []router.Question{
			question("actionable", "The reviewer's findings tell the drift mason what to resolve differently."),
			question("progress", "Successive resolutions got closer to the reviewer's approval.")}
	}
	return []router.Question{
		question("transient", "The failure looks like infrastructure and not like the behaviour of the work."),
		question("repeats", "The same failure repeats across the turns of the drift rebase.")}
}
