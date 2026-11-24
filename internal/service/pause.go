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
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const pauseTool = "pause"

const resumeTool = "resume"

const pauseGuidance = "When the owner asks you to pause or resume work, call pause or resume with scope factory, project, or workstream. For a workstream include its ID; include the target project's ID when it is another project. Give a reason for a pause. These controls are attributed to the owner."

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
		if target.Scope != "factory" && !s.current().Active(target.Project) {
			return priorityRefusal("the requested project is not active")
		}
		if resume {
			err = s.clearPause(target)
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

func (s *Service) setPause(p runtime.Pause) error {
	if err := s.store.SetPause(p); err != nil {
		return err
	}
	s.log().Info("pause set", "scope", p.Target.Scope, "project", string(p.Target.Project), "workstream", string(p.Target.Workstream), "mode", p.Mode, "reason", p.Reason)
	st, _ := s.effective()
	s.turns.stop(st.Pauses)
	return nil
}

func (s *Service) clearPause(target runtime.Target) error {
	if err := s.store.ClearPause(target, runtime.PauseOwner); err != nil {
		return err
	}
	s.log().Info("pause cleared", "scope", target.Scope, "project", string(target.Project), "workstream", string(target.Workstream))
	return nil
}

func pauseStop(p runtime.Pause) *thread.Stop {
	return &thread.Stop{TurnStop: trace.TurnStop{Cause: trace.TurnStopHardPause, Scope: p.Target.Scope, Source: p.Source, Reason: p.Reason}}
}

var pausedActions = map[string]bool{DraftAction: true, AmendmentDraftAction: true, RoundAction: true, ReplyAction: true, RedraftAction: true, AmendmentRoundAction: true, AmendmentReplyAction: true, FinalReviewAction: true, deliveryReviewAction: true, CheckAction: true}

func (s *Service) pausing(project config.ProjectID, stream config.WorkstreamID) (runtime.Pause, bool) {
	if s.store == nil {
		return runtime.Pause{}, false
	}
	st, _ := s.effective()
	return scheduler.Pausing(st.Pauses, project, stream)
}

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

func (s *Service) holding(repository *trace.Repository) func(config.WorkstreamID, coreadapter.Operation) bool {
	cause := s.holdingCause(repository)
	return func(stream config.WorkstreamID, op coreadapter.Operation) bool {
		return cause(stream, op) != scheduler.NotHeld
	}
}

func (s *Service) holdingCause(repository *trace.Repository) func(config.WorkstreamID, coreadapter.Operation) scheduler.HoldCause {
	return func(stream config.WorkstreamID, op coreadapter.Operation) scheduler.HoldCause {
		gone := false
		switch op.Action {
		case DriftAction, LandAction, RebaseAction, PublishAction, publishUpstreamAction, landDeliveryAction:
			var err error
			gone, err = abandoned(repository, stream)
			gone = gone && err == nil
		}
		if op.Action == DriftAction && gone {
			return scheduler.NotHeld
		}
		if role := modelOperationRole(op); role != "" && !s.admitRole(role) {
			return scheduler.HeldByPause
		}
		if gitOperation(op) {
			switch op.Action {
			case LandAction, RebaseAction, PublishAction, publishUpstreamAction, landDeliveryAction:
				if errors.Is(s.held(repository, stream), errPaused) {
					return scheduler.HeldByPause
				}
			}
			if op.Action == landDeliveryAction {
				if why, err := delivery.LandingBusy(repository, stream, op); err != nil || why != "" {
					return scheduler.HeldByOther
				}
			}
			if joining, err := s.gitWorkgroupJoining(repository, stream); err != nil || joining {
				return scheduler.HeldByOther
			}
			if op.Action != SealAction && op.Action != BuildAction && !gone {
				if waiting, err := edgeWaiting(repository, stream); err != nil || waiting {
					return dependencyHold(repository, stream, err)
				}
			}
		}
		if op.Action == deltaTurnAction {
			if errors.Is(s.held(repository, stream), errPaused) {
				return scheduler.HeldByPause
			}
			return scheduler.NotHeld
		}
		if op.Boundary == coreadapter.RepositoryBoundary {
			if held, err := changeRepositoryHeld(repository, stream, op); err != nil || held {
				return scheduler.HeldByOther
			}
		}
		if held, err := changeHeld(repository, stream); err != nil {
			return scheduler.HeldByOther
		} else if held && op.Boundary == coreadapter.RunnerBoundary {
			if input, err := thread.DecodeTurn(op); err == nil {
				t, err := repository.Thread(stream, input.Agent)
				if err == nil && t.Identity.Role == trace.ChiefOfStaff {
					return scheduler.NotHeld
				}
			}
			return scheduler.HeldByOther
		}
		if delivery.PublicationAction(op) && !pulls.Credentialed(s.options.PullRequests) {
			return scheduler.HeldByOther
		}
		if waiting, err := baseWaiting(repository, stream); op.Action != baseRefreshAction && (err != nil || waiting) {
			if input, err := thread.DecodeTurn(op); err == nil {
				t, err := repository.Thread(stream, input.Agent)
				if err == nil && t.Identity.Role == trace.ChiefOfStaff {
					return scheduler.NotHeld
				}
			}
			return dependencyHold(repository, stream, err)
		}
		if pausedActions[op.Action] && op.Boundary == coreadapter.RunnerBoundary && errors.Is(s.held(repository, stream), errPaused) {
			return scheduler.HeldByPause
		}
		return scheduler.NotHeld
	}
}

func dependencyHold(repository *trace.Repository, stream config.WorkstreamID, readErr error) scheduler.HoldCause {
	if readErr != nil {
		return scheduler.HeldByOther
	}
	if approved, err := repository.GroupApproved(stream); err == nil && !approved {
		return scheduler.HeldByCard
	}
	if live, err := liveDependencyWait(repository, stream); err == nil && live {
		return scheduler.HeldByDependency
	}
	return scheduler.HeldByOther
}

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

func (s *Service) continueStopped(ctx context.Context, repository *trace.Repository, cfg *config.Config, role string, q trace.QueuedTurn) (string, error) {
	profile, err := s.agentProfile(cfg, role, q.Request.AgentID)
	if err != nil {
		return "", err
	}
	req := q.Request
	turn := fmt.Sprintf("%s%s%d", intake.ContinuedTurn(q.Request.TurnID), intake.ContinueMark, q.Sequence)
	req.ID, req.TurnID, req.Profile = "request_"+turn, turn, profile
	req.At, req.Cause = s.now(), q.Response.ID
	if !intake.IsContinuation(q.Request.TurnID) {
		req.Prompt += "\n\nA hard pause stopped your last turn. Continue the same work from where it stopped: what you recorded before the pause is kept."
	}
	_, err = repository.EnqueueTurn(ctx, req)
	return turn, err
}
