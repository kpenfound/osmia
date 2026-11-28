package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// The manager tools of a Hill Climb workstream: steering the next
// attempt with a note, and concluding the climb.
const (
	climbSteerTool    = "climb_steer"
	climbConcludeTool = "climb_conclude"
)

func decodeToolInput(raw json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("tool input: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("tool input must be one object")
	}
	return nil
}

func (c *runtimeControls) climbSteer(r *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	return coreadapter.Tool{Name: climbSteerTool, Effect: coreadapter.ToolMemory,
		Description: "Steer the climb's next attempt with a note that says what the mason should try or avoid, based on what the attempt log shows. The note reaches the next attempt, which starts fresh from the current best, and stays in the log of notes that every later attempt reads. It cannot change whether an attempt is kept: Go decides that by arithmetic, so a steering note never claims a score improved. To stop the climb, use climb_conclude. note: at most 2000 characters.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"}},"required":["note"],"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Note string `json:"note"`
			}
			if err := decodeToolInput(raw, &in); err != nil {
				return nil, err
			}
			if err := r.ActiveTurn(scope); err != nil {
				return nil, err
			}
			stream := config.WorkstreamID(scope.Workstream)
			note, err := climb.Steer(ctx, r, stream, in.Note, trace.ManagerKey, now())
			if err != nil {
				return priorityRefusal(err.Error())
			}
			if s := c.service.Load(); s != nil {
				s.kickClimb(r)
			}
			return json.Marshal(map[string]any{"recorded": true, "note": note.Number, "next": "The note is recorded and reaches the mason's next attempt. Tell the owner what you steered."})
		}}
}

func (c *runtimeControls) climbConclude(r *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return coreadapter.Tool{Name: climbConcludeTool, Effect: coreadapter.ToolMemory,
		Description: "Conclude the climb now. No new attempt starts, an attempt in flight is discarded, and the branch goes on to its rebase, final review and delivery. Use it when the attempt log shows the climb has run its course, and use climb_steer instead to keep climbing in a new direction. note: why the climb is concluded, written for the owner.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"}},"required":["note"],"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var in struct {
				Note string `json:"note"`
			}
			if err := decodeToolInput(raw, &in); err != nil {
				return nil, err
			}
			if err := r.ActiveTurn(scope); err != nil {
				return nil, err
			}
			s := c.service.Load()
			if s == nil {
				return nil, errors.New("the service is not running")
			}
			stream := config.WorkstreamID(scope.Workstream)
			state, apiErr := s.climbingState(r, stream)
			if apiErr != nil {
				return priorityRefusal(apiErr.Message)
			}
			actor := trace.Actor{Kind: "agent", ID: scope.Thread}
			res, err := s.RouteExit(ctx, s.concludeExit(r, stream, state, actor, in.Note))
			if err != nil {
				return nil, err
			}
			if !res.Committed {
				return priorityRefusal(fmt.Sprintf("the climb was not concluded: the router held it (%s). %s", res.Hold, res.Reason))
			}
			s.kickClimb(r)
			return json.Marshal(map[string]any{"concluded": true, "state": climb.InnerConcluding, "next": "The climb is concluding and the branch goes on to its rebase, final review and delivery. Tell the owner the climb is concluded."})
		}}
}
