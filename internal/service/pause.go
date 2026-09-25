package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const pauseTool = "pause"
const resumeTool = "resume"
const pauseGuidance = "When the owner asks you to pause or resume work, call pause or resume with scope factory, project, or workstream. For a workstream include its ID; for a project use this project's ID. Give a reason for a pause. These controls are attributed to the owner."

func (c *runtimeControls) pauseControl(repository *trace.Repository, scope coreadapter.Scope, now func() time.Time, resume bool) coreadapter.Tool {
	name := pauseTool
	if resume {
		name = resumeTool
	}
	tool := coreadapter.Tool{Name: name, Effect: coreadapter.ToolMemory,
		Description: "Pause or resume the requested factory, project, or workstream when the owner asks. Pauses require a reason and may be soft or hard.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"scope":{"type":"string","enum":["factory","project","workstream"]},"project":{"type":"string"},"workstream":{"type":"string"},"mode":{"type":"string","enum":["soft","hard"]},"reason":{"type":"string"}},"required":["scope"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Scope      string              `json:"scope"`
			Project    config.ProjectID    `json:"project"`
			Workstream config.WorkstreamID `json:"workstream"`
			Mode       string              `json:"mode"`
			Reason     string              `json:"reason"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		_, owner, err := repository.OwnerTurn(trace.ChiefOfStaff, scope)
		if err != nil {
			return nil, err
		}
		if !owner {
			return priorityRefusal("only an owner message can pause or resume work")
		}
		s := c.service.Load()
		if s == nil {
			return nil, errors.New("runtime pause controls are unavailable")
		}
		target := runtime.Target{Scope: input.Scope, Project: input.Project, Workstream: input.Workstream}
		if target.Scope == "project" && target.Project == "" {
			target.Project = repository.Project()
		}
		if target.Scope == "workstream" && target.Project == "" {
			target.Project = repository.Project()
		}
		if target.Scope != "factory" && target.Project != repository.Project() {
			return priorityRefusal("the requested project is not active")
		}
		if resume {
			err = s.store.ClearPause(target, runtime.PauseOwner)
		} else {
			if strings.TrimSpace(input.Reason) == "" {
				return priorityRefusal("pause reason must be non-empty")
			}
			if input.Mode == "" {
				input.Mode = "soft"
			}
			err = s.setPause(runtime.Pause{Target: target, Mode: input.Mode, Reason: input.Reason, Source: runtime.PauseOwner, SetAt: now()})
		}
		if err != nil {
			if errors.Is(err, runtime.ErrValidation) {
				return priorityRefusal(err.Error())
			}
			return nil, err
		}
		return json.Marshal(struct {
			Recorded bool `json:"recorded"`
		}{true})
	}
	return tool
}

// setPause records the pause, then stops the running turns a hard pause in
// force covers.
func (s *Service) setPause(p runtime.Pause) error {
	if err := s.store.SetPause(p); err != nil {
		return err
	}
	st, _ := s.effective()
	s.turns.stop(st.Pauses)
	return nil
}

// pauseStop is the cause with which the hard pause p stops a turn.
func pauseStop(p runtime.Pause) *thread.Stop {
	return &thread.Stop{TurnStop: trace.TurnStop{Cause: trace.TurnStopHardPause, Scope: p.Target.Scope, Source: p.Source, Reason: p.Reason}}
}

// pausedActions are the operations of the service's reconcilers that run
// architect and committee turns. A pause covering an operation's workstream
// holds it; a hard pause also stops the turn it is running.
var pausedActions = map[string]bool{DraftAction: true, AmendmentDraftAction: true, RoundAction: true, ReplyAction: true, RedraftAction: true, AmendmentRoundAction: true, AmendmentReplyAction: true, FinalReviewAction: true}

// errPaused leaves a reconciler operation pending instead of starting a turn:
// the controller holds the operation until the pause is lifted.
var errPaused = errors.New("a pause holds the workstream")

// pausing returns the pause in force that covers the workstream of the
// project, and whether there is one.
func (s *Service) pausing(project config.ProjectID, stream config.WorkstreamID) (runtime.Pause, bool) {
	if s.store == nil {
		return runtime.Pause{}, false
	}
	st, _ := s.effective()
	return scheduler.Pausing(st.Pauses, project, stream)
}

// held returns errPaused, with the pause, while a pause covers the workstream
// of the repository's project, and nil otherwise. Reconcilers call it before
// they queue or start a turn. An abandoned workstream is not held: its
// operations end as abandonment has them end.
func (s *Service) held(repository *trace.Repository, stream config.WorkstreamID) error {
	p, ok := s.pausing(repository.Project(), stream)
	if !ok {
		return nil
	}
	if gone, err := abandoned(repository, stream); err != nil || gone {
		return err
	}
	return fmt.Errorf("%w: %s pause set by %s: %s", errPaused, p.Target.Scope, p.Source, p.Reason)
}

// holding is the controller's Hold: it holds every reconciler operation that
// runs architect or committee turns while held holds its workstream.
func (s *Service) holding(repository *trace.Repository) func(config.WorkstreamID, coreadapter.Operation) bool {
	return func(stream config.WorkstreamID, op coreadapter.Operation) bool {
		return pausedActions[op.Action] && op.Boundary == coreadapter.RunnerBoundary && errors.Is(s.held(repository, stream), errPaused)
	}
}

// stoppable returns ctx for one reconciler turn of the workstream, held
// until release is called, so that a hard pause covering the workstream stops
// the turn's session the way it stops a scheduled turn. A hard pause already
// in force stops the turn before its session starts.
func (s *Service) stoppable(ctx context.Context, project config.ProjectID, stream config.WorkstreamID) (context.Context, func()) {
	ctx, stop := thread.Stoppable(ctx)
	release := s.turns.track(stream, runningTurn{project: project, stop: stop})
	if s.store != nil {
		st, _ := s.effective()
		if p, ok := scheduler.Stopping(st.Pauses, project, stream); ok {
			stop(pauseStop(p))
		}
	}
	return ctx, release
}

// continueMark separates the turn a pause continuation continues from the
// sequence of the stopped turn in the continuation's ID.
const continueMark = "-continue-"

// continuedTurn returns the turn a pause continuation continues, and any
// other turn itself.
func continuedTurn(turn string) string {
	base, _, _ := strings.Cut(turn, continueMark)
	return base
}

// isContinuation reports whether the turn continues a turn a hard pause
// stopped.
func isContinuation(turn string) bool { return strings.Contains(turn, continueMark) }

// stoppedTurn reports whether a hard pause stopped the completed turn.
func stoppedTurn(q *trace.QueuedTurn) bool {
	return q != nil && !q.CompletedAt.IsZero() && q.Response != nil && q.Response.Stop != nil
}

// continueStopped queues the turn that continues the turn q a hard pause
// stopped, on the same thread and with the role's current profile, so it
// resumes the stopped session, and returns its ID. The continuation belongs
// to the attempt q belongs to: it is named after the turn q continues, and
// spends no attempt.
func (s *Service) continueStopped(ctx context.Context, repository *trace.Repository, cfg *config.Config, role string, q trace.QueuedTurn) (string, error) {
	profile, _, err := s.roleExecution(cfg, role)
	if err != nil {
		return "", err
	}
	req := q.Request
	turn := fmt.Sprintf("%s%s%d", continuedTurn(q.Request.TurnID), continueMark, q.Sequence)
	req.ID, req.TurnID, req.Profile = "request_"+turn, turn, profile
	req.At, req.Cause = s.now(), q.Response.ID
	if !isContinuation(q.Request.TurnID) {
		req.Prompt += "\n\nA hard pause stopped your last turn. Continue the same work from where it stopped: what you recorded before the pause is kept."
	}
	_, err = repository.EnqueueTurn(ctx, req)
	return turn, err
}
