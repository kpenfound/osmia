package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

const loopGuardSubject = "loop-guard"

type loopGuard struct {
	s          *Service
	cfg        *config.Config
	repository *trace.Repository
}

func (g loopGuard) Pass(ctx context.Context) error {
	if g.s.store == nil {
		return nil
	}
	streams, err := intake.NonSessionWorkstreams(g.repository)
	if err != nil {
		return err
	}
	state, _ := g.s.store.Snapshot()
	var costs []trace.Cost
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		guard, err := g.repository.Workflow(stream, loopGuardSubject)
		if err != nil {
			return err
		}
		target := runtime.Target{Scope: "workstream", Project: g.cfg.Project.ID, Workstream: stream}
		held := slices.ContainsFunc(state.Pauses, func(p runtime.Pause) bool { return p.Target == target && p.Source == runtime.PauseLoopGuard })
		if k, paused := strings.CutPrefix(guard.Value, "paused-"); paused {
			if !held {
				if err := g.release(ctx, stream, guard, k); err != nil {
					return err
				}
			}
			continue
		}
		limit := g.cfg.Loop.MaxSessions
		if limit == 0 || scheduler.Paused(state.Pauses, g.cfg.Project.ID, stream) {
			continue
		}
		feature, err := g.repository.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		if feature.Value == "" || intake.Finished(feature.Value) {
			continue
		}
		since, what, err := lastProgress(g.repository, stream)
		if err != nil {
			return err
		}
		if costs == nil {
			if err = g.repository.WithWorkstreamDiscovery(func([]config.WorkstreamID) error {
				var err error
				costs, err = g.repository.Costs()
				return err
			}); err != nil {
				return err
			}
		}
		sessions := sessionsSince(costs, stream, since)
		if sessions < limit {
			continue
		}
		if err := g.pause(ctx, stream, guard, target, sessions, since, what); err != nil {
			return err
		}
	}
	return nil
}

func sessionsSince(costs []trace.Cost, stream config.WorkstreamID, since time.Time) int {
	sessions := 0
	for _, c := range costs {
		if c.Workstream == stream && c.Entry.At.After(since) {
			sessions++
		}
	}
	return sessions
}

func (g loopGuard) pause(ctx context.Context, stream config.WorkstreamID, guard trace.WorkflowState, target runtime.Target, sessions int, since time.Time, what string) error {
	k := 1
	if n, ok := strings.CutPrefix(guard.Value, "released-"); ok {
		if _, err := fmt.Sscanf(n, "%d", &k); err != nil {
			return fmt.Errorf("subject %s is %q", loopGuardSubject, guard.Value)
		}
		k++
	}
	reason := fmt.Sprintf("Loop guard: %d agent sessions ran since the workstream's last progress, %s at %s, reaching loop.max_sessions (%d); everything but the manager waits until the owner resumes the workstream", sessions, what, since.UTC().Format(time.RFC3339), g.cfg.Loop.MaxSessions)
	id := fmt.Sprintf("%s-paused-%d", loopGuardSubject, k)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: g.repository.Project(), Workstream: stream, At: g.s.now(), Actor: loopGuardActor, Cause: loopGuardSubject}
	tx := trace.Transaction{ExpectedVersion: guard.Version, Transition: trace.Transition{Header: h, Subject: loopGuardSubject, From: guard.Value, To: fmt.Sprintf("paused-%d", k), Reason: reason},
		Events: []trace.Event{trace.Notice(id, "loop", reason+".")}}
	if _, err := g.repository.Transact(ctx, tx); errors.Is(err, trace.ErrConflict) {
		return nil
	} else if err != nil {
		return err
	}
	g.s.spans.loopGuardTrip(g.cfg.Project.ID, stream, id, h.At)
	err := g.s.store.SetPause(runtime.Pause{Target: target, Mode: "soft", Reason: reason, Source: runtime.PauseLoopGuard, SetAt: h.At})
	if errors.Is(err, runtime.ErrValidation) {
		return nil
	}
	return err
}

func (g loopGuard) release(ctx context.Context, stream config.WorkstreamID, guard trace.WorkflowState, k string) error {
	id := fmt.Sprintf("%s-released-%s", loopGuardSubject, k)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: g.repository.Project(), Workstream: stream, At: g.s.now(), Actor: loopGuardActor, Cause: fmt.Sprintf("%s-paused-%s", loopGuardSubject, k)}
	tx := trace.Transaction{ExpectedVersion: guard.Version, Transition: trace.Transition{Header: h, Subject: loopGuardSubject, From: guard.Value, To: "released-" + k, Reason: fmt.Sprintf("the loop guard's pause %s of the workstream is gone; its count of sessions without progress starts over", k)}}
	if _, err := g.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

var loopGuardActor = trace.Actor{Kind: "service", ID: loopGuardSubject}

var unitProgress = []string{UnitPlanned, UnitReady, UnitImplementing, UnitWaiting, UnitChecking, UnitReviewing, UnitApproved, UnitMerged}

func lastProgress(repository *trace.Repository, stream config.WorkstreamID) (time.Time, string, error) {
	records, err := trace.Read[trace.Record](repository, stream)
	if err != nil {
		return time.Time{}, "", err
	}
	var at time.Time
	what := "its creation"
	mark := func(t time.Time, why string) {
		if !t.Before(at) {
			at, what = t, why
		}
	}
	for _, record := range records {
		switch r := record.(type) {
		case trace.Transition:
			switch {
			case r.Actor.Kind == ownerActor.Kind:
				mark(r.At, "an owner action")
			case r.From == r.To:
			case r.Subject == trace.FeatureSubject:
				mark(r.At, "the workstream moving to "+featureState(r.To))
			case strings.HasPrefix(r.Subject, "unit-") && r.Actor == managerActor:
			case strings.HasPrefix(r.Subject, "unit-") && slices.Contains(unitProgress, r.To):
				mark(r.At, fmt.Sprintf("unit %s moving to %s", strings.TrimPrefix(r.Subject, "unit-"), r.To))
			case r.Subject == shedSubject, strings.HasPrefix(r.Subject, amendmentSubject("")), r.Subject == finalReviewSubject, r.Subject == publicationSubject:
				mark(r.At, fmt.Sprintf("%s moving to %s", r.Subject, r.To))
			case r.Subject == driftSubject && strings.HasPrefix(r.To, driftRebased+"-"):
				mark(r.At, "a drift rebase")
			case r.Subject == trace.ClimbSubject:
				mark(r.At, "the climb moving to "+r.To)
			case strings.HasPrefix(r.Subject, "climb-attempt-") && r.To == string(climb.PhaseRecorded):
				mark(r.At, "a recorded climb attempt")
			case r.Subject == loopGuardSubject && strings.HasPrefix(r.To, "released-"):
				mark(r.At, "the owner resuming it")
			}
		case trace.Document:
			if r.Actor.Kind == ownerActor.Kind {
				mark(r.At, "an owner action")
			}
			if strings.HasPrefix(r.Path, trace.ClimbAttemptsDir+"/") {
				mark(r.At, "a recorded climb attempt")
			}
		case trace.TurnRequest:
			if r.Actor.Kind == ownerActor.Kind {
				mark(r.At, "an owner message")
			}
		}
	}
	return at, what, nil
}
