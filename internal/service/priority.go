package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

const prioritiseTool = "prioritise"

const priorityGuidance = "## Priority\nWhen the owner asks you to change which workstreams go first, call prioritise with the IDs of the target project's active workstreams in the order the owner wants, highest priority first. Set project to another active project ID for a factory-wide request. " +
	"Workstreams you leave out come after the ones you name. Name each workstream once, and never a delivered or abandoned one. " +
	"prioritise changes the order only: it does not pause, resume or abandon work, and it is the same order the owner sets with osmia priority."

type runtimeControls struct{ service atomic.Pointer[Service] }

func (c *runtimeControls) prioritise(repository *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	tool := coreadapter.Tool{Name: prioritiseTool, Effect: coreadapter.ToolMemory,
		Description: "Set the project's workstream priority order when the owner asks for it in a message. workstreams: the IDs of active workstreams of the target project, highest priority first, each once; workstreams left out come after them. " +
			"Delivered and abandoned workstreams cannot be ordered. The order replaces the previous one and is what osmia status shows; it does not pause, resume or abandon anything.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"project":{"type":"string"},"workstreams":{"type":"array","items":{"type":"string"}}},"required":["workstreams"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Project     config.ProjectID `json:"project"`
			Workstreams []string         `json:"workstreams"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		s := c.service.Load()
		if s == nil {
			return nil, errors.New("the runtime priority is unavailable")
		}
		project := input.Project
		if project == "" {
			project = repository.Project()
		}
		target, err := s.repository(project)
		if err != nil {
			return priorityRefusal("the requested project is not active")
		}
		order, reason, err := checkPriority(target, project, input.Workstreams)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return priorityRefusal(reason)
		}
		previous, stored := storedPriority(s.store, project)
		applied := false
		targetProject := project
		if project == repository.Project() {
			targetProject = ""
		}
		change, err := repository.SetProjectPriority(ctx, trace.Manager, scope, targetProject, now(), func() ([]config.WorkstreamID, error) {
			if err := s.store.SetPriority(runtime.Priority{Project: project, Workstreams: order}); err != nil {
				switch {
				case errors.Is(err, runtime.ErrValidation):
					return nil, &trace.PriorityRefused{Reason: "the runtime settings refused the order: " + err.Error()}
				case errors.Is(err, runtime.ErrConflict):
					return nil, &trace.PriorityRefused{Reason: "the runtime settings changed outside Osmia; the owner must restore them or restart the service before the order can change"}
				}
				return nil, err
			}
			applied = true
			return effectivePriority(s.store, project), nil
		})
		var refused *trace.PriorityRefused
		if errors.As(err, &refused) {
			return priorityRefusal(refused.Reason)
		}
		if err != nil {
			if applied {
				restore := s.store.ClearPriority(project)
				if stored {
					restore = s.store.SetPriority(previous)
				}
				err = errors.Join(err, restore)
			}
			return nil, err
		}
		return json.Marshal(struct {
			Recorded    bool                  `json:"recorded"`
			Workstreams []config.WorkstreamID `json:"workstreams"`
		}{true, change.Order})
	}
	return tool
}

func checkPriority(repository *trace.Repository, project config.ProjectID, order []string) ([]config.WorkstreamID, string, error) {
	if len(order) == 0 {
		return nil, "workstreams must name at least one active workstream", nil
	}
	known, err := repository.Workstreams()
	if err != nil {
		return nil, "", err
	}
	out := make([]config.WorkstreamID, 0, len(order))
	for _, raw := range order {
		id, err := config.ParseWorkstreamID(raw)
		if err != nil {
			return nil, fmt.Sprintf("%q is not a workstream ID: w_ followed by 32 lowercase hexadecimal digits", raw), nil
		}
		if slices.Contains(out, id) {
			return nil, fmt.Sprintf("workstream %s is listed more than once", id), nil
		}
		if !slices.Contains(known, id) {
			return nil, fmt.Sprintf("workstream %s is not a workstream of this project", id), nil
		}
		state, err := repository.Workflow(id, trace.FeatureSubject)
		if err != nil {
			return nil, "", err
		}
		if intake.Finished(state.Value) {
			return nil, fmt.Sprintf("workstream %s is %s; only active workstreams have a priority", id, state.Value), nil
		}
		out = append(out, id)
	}
	return out, "", nil
}

func storedPriority(store *runtime.Store, project config.ProjectID) (runtime.Priority, bool) {
	st, _ := store.Snapshot()
	for _, p := range st.Priorities {
		if p.Project == project {
			return p, true
		}
	}
	return runtime.Priority{}, false
}

func effectivePriority(store *runtime.Store, project config.ProjectID) []config.WorkstreamID {
	st, _ := store.Effective()
	for _, p := range st.Priorities {
		if p.Project == project {
			return p.Workstreams
		}
	}
	return []config.WorkstreamID{}
}
