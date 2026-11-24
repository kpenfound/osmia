package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// The chief of staff's recovery tools.
const (
	DropDependencyTool = "drop_dependency"
	RetryChangeTool    = "retry_change"
)

func decodeOne(raw json.RawMessage, v any) error {
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

// DropDependency returns the drop_dependency tool of one claimed chief-of-staff
// turn. The chief of staff drops a dependency of a member that waits on an
// upstream that will not merge or no longer matters, as its own decision: the
// same waiver the owner's drop records, with the chief of staff as the decider.
func (c *Controls) DropDependency(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: DropDependencyTool, Effect: coreadapter.ToolMemory,
		Description: "Drop one dependency of a workstream that waits on an upstream workstream: a cross-repo edge, or the same-repo base it is parked on. Use it when the upstream will not merge, its changes already reached upstream another way, or the dependent works without it. The drop is your decision and is recorded as a waiver; it never edits the shared project spec and is never merge evidence. upstream: the upstream workstream ID. type: the edge type, or base, when the upstream is named by more than one dependency. reason: why the dependent can go on. workstream: the dependent member; omit for this conversation's own.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"upstream":{"type":"string"},"type":{"type":"string"},"reason":{"type":"string"},"workstream":{"type":"string"}},"required":["upstream","reason"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Upstream   config.WorkstreamID `json:"upstream"`
			Type       string              `json:"type"`
			Reason     string              `json:"reason"`
			Workstream config.WorkstreamID `json:"workstream"`
		}
		if err := decodeOne(raw, &input); err != nil {
			return nil, err
		}
		s := c.Load()
		if s == nil {
			return nil, errors.New("dependencies are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		target, stream, refusal, err := repository.ChiefMember(config.WorkstreamID(scope.Workstream), input.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return priorityRefusal(refusal)
		}
		authority := &router.Authority{Kind: router.AuthorityChief, Target: DependencyWaived, Ref: "chief of staff: " + input.Reason}
		out, api := s.dropDependency(ctx, target.Project(), stream, target, DropDependencyRequest{Upstream: input.Upstream, Type: input.Type}, chiefActor, authority)
		if api != nil {
			if api.Code == Internal {
				return nil, errors.New(api.Message)
			}
			return priorityRefusal(api.Message)
		}
		return json.Marshal(struct {
			Recorded   bool                `json:"recorded"`
			Workstream config.WorkstreamID `json:"workstream"`
			Upstream   config.WorkstreamID `json:"upstream"`
			Type       string              `json:"type"`
		}{true, out.Workstream, out.Upstream, out.Type})
	}
	return tool
}

// RetryChange returns the retry_change tool of one claimed chief-of-staff turn:
// it starts a stuck workgroup change's drafting and debate again.
func (c *Controls) RetryChange(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: RetryChangeTool, Effect: coreadapter.ToolMemory,
		Description: "Start a stuck workgroup change's drafting and debate again: a change still drafting, debating or answering whose turn failed or never ran, or one that failed before it was presented. It returns to drafting with its hold cleared, and the architect drafts it again on the next pass. change: the change ID request_change returned.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"change":{"type":"string"}},"required":["change"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Change string `json:"change"`
		}
		if err := decodeOne(raw, &input); err != nil {
			return nil, err
		}
		s := c.Load()
		if s == nil {
			return nil, errors.New("workgroup changes are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		out, api := s.RetryChange(ctx, config.WorkstreamID(scope.Workstream), input.Change)
		if api != nil {
			if api.Code == Internal {
				return nil, errors.New(api.Message)
			}
			return priorityRefusal(api.Message)
		}
		return json.Marshal(struct {
			Recorded bool   `json:"recorded"`
			Change   string `json:"change"`
			State    string `json:"state"`
			Attempt  int    `json:"attempt"`
		}{true, out.ID, out.State, out.Attempt})
	}
	return tool
}
