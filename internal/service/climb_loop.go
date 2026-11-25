package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	kindreg "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// The router machine of a Hill Climb workstream's climbing exits, their
// triggers and their targets.
const (
	climbMachine        = "climb"
	triggerClimbStop    = "climb.stop"
	triggerClimbConcl   = "climb.conclude"
	triggerClimbExtend  = "climb.extend"
	targetClimbConclude = "conclude"
	targetClimbContinue = "continue"
	targetClimbExtend   = "extend"
)

func (s *Service) climbLoop(repository *trace.Repository) *climb.Loop {
	cfg := s.about(repository)
	root := filepath.Join(cfg.Root.String(), "climb", string(repository.Project()))
	return &climb.Loop{
		Repository: repository,
		Spec: func(stream config.WorkstreamID) (climb.Spec, int, error) {
			spec, sealed, err := intake.SealedClimbSpec(repository, cfg, stream)
			return spec, sealed.Revision.Spec, err
		},
		SpecAt: climbSpecReader(repository, cfg),
		Branch: intake.FeatureBranch,
		Host: func(stream config.WorkstreamID) (climb.Host, error) {
			return s.execution().ClimbHost(repository, stream)
		},
		Runner: func(stream config.WorkstreamID, source climb.Source) (climb.Runner, error) {
			engine, ok := s.configuredChecks(cfg).(checks.FunctionCaller)
			if !ok {
				return climb.Runner{}, errors.New("the configured check client cannot call a function of the dev module")
			}
			project := repository.Project()
			return climb.Runner{Source: source, Engine: engine, Root: filepath.Join(root, "measure"), Timeout: cfg.HillClimb().MeasurementTimeout,
				AcquireSlot: func(ctx context.Context) (func(), error) {
					return s.checkSlots.acquire(ctx, CheckSlotRun{Project: project, Workstream: stream, Unit: "measure"})
				}}, nil
		},
		Spent: func(stream config.WorkstreamID, since time.Time) (float64, error) {
			costs, err := trace.Read[trace.Cost](repository, stream)
			if err != nil {
				return 0, err
			}
			var recent []trace.Cost
			for _, c := range costs {
				if c.Entry.At.After(since) {
					recent = append(recent, c)
				}
			}
			total, err := sumCosts(recent)
			if err != nil {
				return 0, err
			}
			spent, _ := total.known.Float64()
			return spent, nil
		},
		Allow: func(ctx context.Context, stream config.WorkstreamID) (string, error) {
			return s.routeStops(ctx, router.Exit{Repo: repository, Project: repository.Project(), Workstream: stream})
		},
		Stop: func(ctx context.Context, req climb.StopRequest) (climb.StopOutcome, error) {
			return s.raiseClimbStop(ctx, repository, req)
		},
		Publish: func(kind api.EventKind, stream config.WorkstreamID) {
			s.hub.Publish(api.Event{Kind: kind, Project: repository.Project(), Workstream: stream})
		},
		Now:  s.now,
		Step: s.step,
	}
}

func stopText(firings []climb.Firing) string {
	parts := make([]string, len(firings))
	for i, f := range firings {
		parts[i] = f.Rule + ": " + f.Reason
	}
	return strings.Join(parts, "; ")
}

func (s *Service) raiseClimbStop(ctx context.Context, repository *trace.Repository, req climb.StopRequest) (climb.StopOutcome, error) {
	hard := climb.Hard(req.Firings)
	why := stopText(req.Firings)
	id := fmt.Sprintf("climb-stop-%d", req.After)
	rules := climb.Rules(req.Firings)
	var records []router.Entry
	attempts, err := climb.ReadAttempts(repository, req.Stream)
	if err != nil {
		return "", err
	}
	for _, a := range attempts[max(0, len(attempts)-5):] {
		records = append(records, router.Entry{Kind: "attempt", ID: climb.AttemptUnit(a.Number), Text: fmt.Sprintf("attempt %d %s", a.Number, strings.TrimSpace(fmt.Sprintf("%s %s %s", a.Outcome, a.Cause, oneLineText(a.Hypothesis))))})
	}
	baseline, epoch := 0.0, 1
	if approval, ok, err := climb.ReadApproval(repository, req.Stream); err == nil && ok {
		baseline, epoch = approval.Baseline, approval.EpochNumber()
	}
	best, _ := climb.Best(baseline, climb.InEpoch(attempts, epoch))
	kind := "plateau"
	if hard {
		kind = "budget"
	}
	x := router.Exit{
		Repo: repository, Project: repository.Project(), Workstream: req.Stream, Machine: climbMachine,
		Subject: climb.InnerSubject, From: req.State.Value, Version: req.State.Version,
		Trigger: triggerClimbStop, Cause: fmt.Sprintf("stop-%d", req.After), HeaderCause: trace.ClimbJournalID(req.After), Actor: climb.InnerActor, At: s.now(),
		Default: targetClimbConclude,
		Targets: []router.Target{
			{Name: targetClimbConclude, State: climb.InnerConcluding, Description: "End the climb: no attempt starts again, and the branch goes on to its rebase, final review and delivery."},
			{Name: targetClimbContinue, NoMove: true, Bar: router.BarStrict, Works: true, Description: "Let the climb go on past the rule that fired: more attempts start. Legal only for a soft rule; a spent budget needs the owner's extension first.",
				Corroborate: []router.Condition{{Question: "worth_continuing", AtLeast: router.LevelStrict}}},
		},
		Questions:    []router.Question{{Name: "worth_continuing", Instructions: "The recorded attempts show that more attempts are likely to improve the primary metric by at least the minimum improvement: ideas remain untried, or the scores are still moving toward the target."}},
		TransitionID: func(router.Target) string { return id },
		Enter: func(context.Context, router.Plan) (router.Effect, error) {
			body := fmt.Sprintf("The climb concludes after attempt %d: %s. The best score is %v. No attempt starts again; the branch goes on to its rebase and delivery.", req.After, why, best)
			return router.Effect{Events: []trace.Event{trace.Notice(id+"-conclude", "chief", body)}, Reason: fmt.Sprintf("a stop rule fired after attempt %d (%s), so the climb concludes", req.After, why)}, nil
		},
		Entering: map[string]router.EffectFunc{targetClimbContinue: func(context.Context, router.Plan) (router.Effect, error) {
			doc, err := climb.StopDocument(repository, req.Stream, req.After, rules, s.now())
			if err != nil {
				return router.Effect{}, err
			}
			body := fmt.Sprintf("The climb goes on past a %s stop after attempt %d: %s. The best score is %v.", kind, req.After, why, best)
			return router.Effect{Documents: []trace.Document{doc}, Events: []trace.Event{trace.Notice(id+"-continue", "chief", body)}, Reason: fmt.Sprintf("Jev judged more attempts worth making past the rule that fired after attempt %d (%s)", req.After, why)}, nil
		}},
		Evidence: router.Evidence{
			Intent:  "The climb improves one measured number by repeated attempts that Go keeps or discards by arithmetic; a stop rule fired.",
			Facts:   map[string]any{"after_attempt": req.After, "rules": rules, "hard": hard, "best": best, "baseline": baseline},
			Records: append([]router.Entry{{Kind: "stop rules", Text: why}}, records...),
		},
		Prereq: router.Prerequisites{Other: func(_ context.Context, t router.Target) (string, error) {
			if t.Name == targetClimbContinue && hard {
				return "an attempt, cost or time budget is spent, and only an owner budget extension lets the climb go on past it", nil
			}
			return "", nil
		}},
	}
	res, err := s.RouteExit(ctx, x)
	if err != nil {
		return "", err
	}
	if !res.Reused {
		event := api.EventClimbPlateau
		if hard {
			event = api.EventClimbStop
		}
		s.hub.Publish(api.Event{Kind: event, Project: repository.Project(), Workstream: req.Stream})
	}
	switch {
	case res.Committed && res.Target == targetClimbConclude:
		return climb.StopConclude, nil
	case res.Committed && res.Target == targetClimbContinue:
		return climb.StopContinue, nil
	}
	return climb.StopWait, nil
}

func oneLineText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return s
}

func (s *Service) climbingState(repository *trace.Repository, stream config.WorkstreamID) (trace.WorkflowState, *APIError) {
	if kind, err := repository.Kind(stream); err != nil {
		return trace.WorkflowState{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the kind of workstream %s; check the trace repository", stream)}
	} else if kind != kindreg.HillClimb {
		return trace.WorkflowState{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is not a %s workstream, so it has no climb to control", stream, kindreg.HillClimb)}
	}
	state, err := climb.ReadInner(repository, stream)
	if err != nil {
		return trace.WorkflowState{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the climb of workstream %s: %v", stream, err)}
	}
	if state.Value != climb.InnerClimbing {
		return trace.WorkflowState{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the climb of workstream %s is %q, not climbing", stream, state.Value)}
	}
	return state, nil
}

func (s *Service) concludeExit(repository *trace.Repository, stream config.WorkstreamID, state trace.WorkflowState, by trace.Actor, note string) router.Exit {
	id := "climb-concluded"
	authority := router.AuthorityOwner
	if by.Kind != intake.OwnerActor.Kind {
		authority = router.AuthorityChief
	}
	who := "the owner"
	if authority == router.AuthorityChief {
		who = "the chief of staff"
	}
	body := fmt.Sprintf("The climb was concluded now by %s. No attempt starts again; an attempt in flight is discarded, and the branch goes on to its rebase and delivery.", who)
	if strings.TrimSpace(note) != "" {
		body += " Note: " + strings.TrimSpace(note)
	}
	return router.Exit{
		Repo: repository, Project: repository.Project(), Workstream: stream, Machine: climbMachine,
		Subject: climb.InnerSubject, From: state.Value, Version: state.Version,
		Trigger: triggerClimbConcl, Cause: "conclude-now", HeaderCause: "conclude-now", Actor: by, Note: note, At: s.now(),
		Default: targetClimbConclude, Authority: &router.Authority{Kind: authority, Target: targetClimbConclude, Ref: "conclude-now"},
		Targets:      []router.Target{{Name: targetClimbConclude, State: climb.InnerConcluding, Description: "End the climb now.", Reason: "the climb was concluded now by " + who}},
		TransitionID: func(router.Target) string { return id },
		Enter: func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{Events: []trace.Event{trace.Notice(id, "chief", body)}}, nil
		},
	}
}

func (s *Service) concludeClimb(ctx context.Context, raw string, req api.ClimbConcludeRequest) (api.ClimbConcludeResponse, *APIError) {
	project, stream, repository, traceErr := s.conversationTrace(raw)
	if traceErr != nil {
		return api.ClimbConcludeResponse{}, traceErr
	}
	state, apiErr := s.climbingState(repository, stream)
	if apiErr != nil {
		return api.ClimbConcludeResponse{}, apiErr
	}
	res, err := s.RouteExit(ctx, s.concludeExit(repository, stream, state, intake.OwnerActor, req.Note))
	if apiErr := intake.RouteError(res, err, "the conclusion of the climb"); apiErr != nil {
		return api.ClimbConcludeResponse{}, apiErr
	}
	s.kickClimb(repository)
	return api.ClimbConcludeResponse{Project: project, Workstream: stream, State: climb.InnerConcluding}, nil
}

func (s *Service) extendClimb(ctx context.Context, raw string, req api.ClimbExtendRequest) (api.ClimbExtendResponse, *APIError) {
	project, stream, repository, traceErr := s.conversationTrace(raw)
	if traceErr != nil {
		return api.ClimbExtendResponse{}, traceErr
	}
	ext := climb.Extension{Attempts: req.Attempts, CostUSD: req.CostUSD, WallClock: req.WallClock, Note: strings.TrimSpace(req.Note)}
	if problems := ext.Problems(); len(problems) > 0 {
		return api.ClimbExtendResponse{}, &APIError{Code: Validation, Message: "the extension is refused: " + strings.Join(problems, "; ")}
	}
	state, apiErr := s.climbingState(repository, stream)
	if apiErr != nil {
		return api.ClimbExtendResponse{}, apiErr
	}
	budget, err := climb.ReadBudget(repository, stream)
	if err != nil {
		return api.ClimbExtendResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the budget of workstream %s: %v", stream, err)}
	}
	n := len(budget.Extensions) + 1
	id := fmt.Sprintf("climb-extended-%d", n)
	var granted climb.Extension
	x := router.Exit{
		Repo: repository, Project: repository.Project(), Workstream: stream, Machine: climbMachine,
		Subject: climb.InnerSubject, From: state.Value, Version: state.Version,
		Trigger: triggerClimbExtend, Cause: fmt.Sprintf("extend-%d", n), HeaderCause: id, Actor: intake.OwnerActor, At: s.now(),
		Default: targetClimbExtend, Authority: &router.Authority{Kind: router.AuthorityOwner, Target: targetClimbExtend, Ref: id},
		Targets:      []router.Target{{Name: targetClimbExtend, NoMove: true, Description: "Extend the climb's budget.", Reason: "the owner extended the climb's budget"}},
		TransitionID: func(router.Target) string { return id },
		Enter: func(context.Context, router.Plan) (router.Effect, error) {
			doc, e, err := climb.BudgetDocument(repository, stream, ext, s.now())
			if err != nil {
				return router.Effect{}, err
			}
			granted = e
			body := fmt.Sprintf("The owner extended the climb's budget: %s.", extensionText(e))
			return router.Effect{Documents: []trace.Document{doc}, Events: []trace.Event{trace.Progress(id, "chief", body)}, Reason: "the owner extended the climb's budget: " + extensionText(e)}, nil
		},
	}
	res, err := s.RouteExit(ctx, x)
	if apiErr := intake.RouteError(res, err, "the budget extension"); apiErr != nil {
		return api.ClimbExtendResponse{}, apiErr
	}
	s.kickClimb(repository)
	if granted.Number == 0 {
		granted.Number, granted.Attempts, granted.CostUSD, granted.WallClock = n, ext.Attempts, ext.CostUSD, ext.WallClock
	}
	return api.ClimbExtendResponse{Project: project, Workstream: stream, Extension: granted.Number, Attempts: granted.Attempts, CostUSD: granted.CostUSD, WallClock: granted.WallClock}, nil
}

func extensionText(e climb.Extension) string {
	var parts []string
	if e.Attempts > 0 {
		parts = append(parts, fmt.Sprintf("%d more attempts", e.Attempts))
	}
	if e.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f more cost", e.CostUSD))
	}
	if e.WallClock != "" {
		parts = append(parts, e.WallClock+" more time")
	}
	return strings.Join(parts, ", ")
}

func climbProgressText(repository *trace.Repository, stream config.WorkstreamID, spec climb.Spec, now time.Time) (string, error) {
	inner, err := climb.ReadInner(repository, stream)
	if err != nil {
		return "", err
	}
	attempts, err := climb.ReadAttempts(repository, stream)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Climb progress\n\nThe climb is %s.\n", inner.Value)
	approval, approved, err := climb.ReadApproval(repository, stream)
	if err != nil {
		return "", err
	}
	kept := 0
	for _, a := range attempts {
		if a.Outcome == climb.OutcomeKept {
			kept++
		}
	}
	fmt.Fprintf(&b, "\n%d attempts recorded, %d kept.\n", len(attempts), kept)
	if approved {
		best, _ := climb.Best(approval.Baseline, climb.InEpoch(attempts, approval.EpochNumber()))
		change := best - approval.Baseline
		line := fmt.Sprintf("%s: baseline %v, best %v (%+v)", spec.Metric.Name, approval.Baseline, best, change)
		if approval.Baseline != 0 {
			line += fmt.Sprintf(", %+.1f%%", change/math.Abs(approval.Baseline)*100)
		}
		fmt.Fprintf(&b, "\n%s; %s is better; noise %v.\n", line, spec.Metric.Direction, approval.Noise)
		budget, err := climb.ReadBudget(repository, stream)
		if err != nil {
			return "", err
		}
		total := budget.Total()
		fmt.Fprintf(&b, "\nAttempts: %d of %d allowed.", len(attempts), spec.Stop.MaxAttempts+total.Attempts)
		if d, ok := spec.WallClock(); ok {
			fmt.Fprintf(&b, " Time: %s of %s.", now.Sub(approval.StartedAt()).Round(time.Minute), d+total.WallClock)
		}
		fmt.Fprintln(&b)
	}
	if run, found, err := climb.ReadRunning(repository, stream); err != nil {
		return "", err
	} else if found && run.Phase != "" {
		phase := run.Phase
		if run.Phase == kindreg.PhaseMeasuring {
			phase = fmt.Sprintf("measuring %d of %d", run.Run, run.Runs)
		}
		fmt.Fprintf(&b, "\nAttempt %d is running: %s. Hypothesis: %s\n", run.Attempt, phase, oneLineText(run.Hypothesis))
	}
	return b.String(), nil
}

// climbSpecReader reads an earlier revision of a workstream's climb spec,
// which the inner machine and the loop compare with the sealed one.
func climbSpecReader(repository *trace.Repository, cfg *config.Config) climb.SpecReader {
	return func(stream config.WorkstreamID, revision int) (climb.Spec, error) {
		spec, _, err := climb.SealedSpec(repository, stream, revision, intake.ClimbDefaults(cfg))
		return spec, err
	}
}

func climbSpecOf(repository *trace.Repository, stream config.WorkstreamID) (climb.Spec, int, error) {
	sealed, _, found, err := seal.Latest(repository, stream)
	if err != nil {
		return climb.Spec{}, 0, err
	}
	if !found {
		return climb.Spec{}, 0, fmt.Errorf("workstream %s is not sealed", stream)
	}
	spec, _, err := climb.SealedSpec(repository, stream, sealed.Revision.Spec, climb.Defaults{})
	return spec, sealed.Revision.Spec, err
}
