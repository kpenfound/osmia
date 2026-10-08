package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

// loopGuardSubject is the workflow subject that tracks the loop guard's
// pauses of a workstream: paused-<k> once it sets pause k, then released-<k>
// once the pause is gone.
const loopGuardSubject = "loop-guard"

// loopGuard pauses a workstream that runs loop.max_sessions agent sessions
// without progress, with a soft pause attributed to the loop guard and a
// notice, and records the pause and its release in the workstream's trace.
// A pause anyone else set on the workstream, its project or the factory
// holds the guard back, and a released pause counts as progress, so the
// count starts over once the owner resumes the workstream.
type loopGuard struct {
	s          *Service
	cfg        *config.Config
	repository *trace.Repository
}

func (g loopGuard) Pass(ctx context.Context) error {
	if g.s.store == nil {
		return nil
	}
	streams, err := g.repository.Workstreams()
	if err != nil {
		return err
	}
	state, _ := g.s.store.Snapshot()
	var costs []trace.Cost
	librarian := librarianWorkstream(g.repository.Project())
	for _, stream := range streams {
		if stream == librarian {
			continue
		}
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
		if feature.Value == "" || feature.Value == DeliveredState || feature.Value == AbandonedState {
			continue
		}
		since, what, err := lastProgress(g.repository, stream)
		if err != nil {
			return err
		}
		if costs == nil {
			if costs, err = g.repository.Costs(); err != nil {
				return err
			}
		}
		sessions := 0
		for _, c := range costs {
			if c.Workstream == stream && c.Entry.At.After(since) {
				sessions++
			}
		}
		if sessions < limit {
			continue
		}
		if err := g.pause(ctx, stream, guard, target, sessions, since, what); err != nil {
			return err
		}
	}
	return nil
}

// pause records the loop guard's next pause of the workstream, with a notice
// for the chief of staff, then sets it. A pause set on the workstream since
// the snapshot stays, and the guard records the release on its next pass.
func (g loopGuard) pause(ctx context.Context, stream config.WorkstreamID, guard trace.WorkflowState, target runtime.Target, sessions int, since time.Time, what string) error {
	k := 1
	if n, ok := strings.CutPrefix(guard.Value, "released-"); ok {
		if _, err := fmt.Sscanf(n, "%d", &k); err != nil {
			return fmt.Errorf("subject %s is %q", loopGuardSubject, guard.Value)
		}
		k++
	}
	reason := fmt.Sprintf("Loop guard: %d agent sessions ran since the workstream's last progress, %s at %s, reaching loop.max_sessions (%d); everything but the chief of staff waits until the owner resumes the workstream", sessions, what, since.UTC().Format(time.RFC3339), g.cfg.Loop.MaxSessions)
	id := fmt.Sprintf("%s-paused-%d", loopGuardSubject, k)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: g.repository.Project(), Workstream: stream, At: g.s.now(), Actor: loopGuardActor, Cause: loopGuardSubject}
	tx := trace.Transaction{ExpectedVersion: guard.Version, Transition: trace.Transition{Header: h, Subject: loopGuardSubject, From: guard.Value, To: fmt.Sprintf("paused-%d", k), Reason: reason},
		Events: []trace.Event{trace.Notice(id, "loop", reason+".")}}
	if _, err := g.repository.Transact(ctx, tx); errors.Is(err, trace.ErrConflict) {
		return nil
	} else if err != nil {
		return err
	}
	err := g.s.store.SetPause(runtime.Pause{Target: target, Mode: "soft", Reason: reason, Source: runtime.PauseLoopGuard, SetAt: h.At})
	if errors.Is(err, runtime.ErrValidation) {
		return nil
	}
	return err
}

// release records that the loop guard's pause k of the workstream is gone.
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

// unitProgress are the states a unit moves into as it makes progress.
var unitProgress = []string{UnitPlanned, UnitReady, UnitImplementing, UnitWaiting, UnitChecking, UnitReviewing, UnitApproved, UnitMerged}

// lastProgress returns when the workstream last made progress, and what that
// was: its creation; anything the owner recorded; a change of state of the
// feature, a unit (other than into contested), the shed, an amendment, the
// final review or the publication; a drift rebase that moved the feature
// branch; or the release of a loop guard pause.
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
			case strings.HasPrefix(r.Subject, "unit-") && r.Actor == chiefActor:
				// A recovery instruction is not evidence of progress.
			case strings.HasPrefix(r.Subject, "unit-") && slices.Contains(unitProgress, r.To):
				mark(r.At, fmt.Sprintf("unit %s moving to %s", strings.TrimPrefix(r.Subject, "unit-"), r.To))
			case r.Subject == shedSubject, strings.HasPrefix(r.Subject, amendmentSubject("")), r.Subject == finalReviewSubject, r.Subject == publicationSubject:
				mark(r.At, fmt.Sprintf("%s moving to %s", r.Subject, r.To))
			case r.Subject == driftSubject && strings.HasPrefix(r.To, driftRebased+"-"):
				mark(r.At, "a drift rebase")
			case r.Subject == loopGuardSubject && strings.HasPrefix(r.To, "released-"):
				mark(r.At, "the owner resuming it")
			}
		case trace.Document:
			if r.Actor.Kind == ownerActor.Kind {
				mark(r.At, "an owner action")
			}
		case trace.TurnRequest:
			if r.Actor.Kind == ownerActor.Kind {
				mark(r.At, "an owner message")
			}
		}
	}
	return at, what, nil
}
