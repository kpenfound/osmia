package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service/beekeeper"
)

// listFactoryTool lists every registered project and each project's
// workstreams with their current status, so the Beekeeper knows which
// chiefs of staff it can talk to.
const listFactoryTool = "list_factory"

// FactoryProjectStatus is one registered project in the Beekeeper's
// list-the-factory tool result: its identity and its workstreams with their
// current status, in the order the service's configuration registers
// projects.
type FactoryProjectStatus struct {
	Project     config.ProjectID   `json:"project"`
	Name        string             `json:"name"`
	Workstreams []WorkstreamStatus `json:"workstreams"`
}

// FactoryList is the list-the-factory tool's result: every registered
// project, never the shadow project, each with its workstreams.
type FactoryList struct {
	Projects []FactoryProjectStatus `json:"projects"`
}

// factoryList reuses statuses, the same status query the HTTP API and the
// web UI read, and groups its entries by registered project, in the
// configured project order. The shadow project is never registered, so
// statuses already leaves it and its one reserved workstream out. No owner
// project registered reports an empty list of projects, not an error.
func (s *Service) factoryList() (FactoryList, *APIError) {
	cfg := s.current()
	list, _, api := s.statuses()
	if api != nil {
		return FactoryList{}, api
	}
	byProject := map[config.ProjectID][]WorkstreamStatus{}
	for _, w := range list {
		byProject[w.Project] = append(byProject[w.Project], w)
	}
	out := FactoryList{Projects: []FactoryProjectStatus{}}
	for _, p := range cfg.Projects {
		workstreams := byProject[p.ID]
		if workstreams == nil {
			workstreams = []WorkstreamStatus{}
		}
		out.Projects = append(out.Projects, FactoryProjectStatus{Project: p.ID, Name: p.Name, Workstreams: workstreams})
	}
	return out, nil
}

// listFactory is the Beekeeper's one read-only "list the factory" tool: it
// takes no input, reads no files, and grants no write capability, version
// control or credentials. Only a claimed turn of the Beekeeper's own thread
// may call it; any other scope is refused and nothing is read.
func (c *runtimeControls) listFactory(scope coreadapter.Scope) coreadapter.Tool {
	return coreadapter.Tool{
		Name:        listFactoryTool,
		Effect:      coreadapter.ToolRead,
		Description: "List every registered project and each project's workstreams with their current status. Never lists the Beekeeper's own shadow project. Takes no input.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if scope.Role != beekeeper.AgentID {
				return nil, errors.New("list_factory is granted to the beekeeper only")
			}
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			var empty struct{}
			if err := d.Decode(&empty); err != nil {
				return nil, err
			}
			if err := d.Decode(new(any)); err != io.EOF {
				return nil, errors.New("list_factory input must be one object")
			}
			s := c.service.Load()
			if s == nil {
				return nil, errors.New("the service is not ready")
			}
			if err := s.Beekeeper().ActiveTurn(scope); err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			list, api := s.factoryList()
			if api != nil {
				return nil, api
			}
			return json.Marshal(list)
		},
	}
}
