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
	st, _ := s.store.Effective()
	s.turns.stop(st.Pauses)
	return nil
}

// pauseStop is the cause with which the hard pause p stops a turn.
func pauseStop(p runtime.Pause) *thread.Stop {
	return &thread.Stop{TurnStop: trace.TurnStop{Cause: trace.TurnStopHardPause, Scope: p.Target.Scope, Source: p.Source, Reason: p.Reason}}
}
