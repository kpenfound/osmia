package isolation

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// Selection comes from the service's workspace and role bindings. Narrow may
// remove permissions for a turn; it cannot add to the service's role grant.
type Selection struct {
	Workspace   coreadapter.WorkspaceRequest
	Paths       []string
	Execution   coreadapter.ExecutionSettings
	Narrow      *coreadapter.Capabilities
	Environment map[string]string
}

// Turns prepares resources after the durable thread claims its turn, so setup
// failures are captured through the same owned response path as execution errors.
// Grants, Tools, Select and Hosts are service configuration, never repo input.
// Tools are trusted handlers; they must enforce their declared effects and scope.
type Turns struct {
	Workspaces coreadapter.Workspaces
	Views      Views
	Select     func(context.Context, coreadapter.Scope) (Selection, error)
	Grants     map[string]coreadapter.Capabilities
	Tools      []coreadapter.Tool
	// Scoped supplies trusted handlers bound to one claimed turn, such as private
	// role notes. They join the same registry and grant checks as Tools.
	Scoped func(context.Context, coreadapter.Scope) ([]coreadapter.Tool, error)
	Hosts  func(token string) coreadapter.MCPHosts
	Engine coreadapter.Engine
	// Capture may snapshot selected output through the service before cleanup.
	// It runs even on execution errors, but never grants an agent VCS access.
	Capture func(context.Context, coreadapter.Scope, *FileView, coreadapter.SessionResult) error
}

var _ coreadapter.Turns = (*Turns)(nil)
var _ coreadapter.ResumeChecker = (*Turns)(nil)

// CheckResume defers to the execution engine through the same executor
// that runs the turn, so continuation never bypasses service isolation.
func (r *Turns) CheckResume(ctx context.Context, previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
	if r.Engine == nil {
		return errors.New("no core execution engine supplied")
	}
	return (&coreadapter.TurnRunner{Executor: coreadapter.CoreExecutor{Runner: r.Engine}}).CheckResume(ctx, previous, next, session)
}

func narrow(grant coreadapter.Capabilities, request *coreadapter.Capabilities) coreadapter.Capabilities {
	grant.Tools = slices.Clone(grant.Tools)
	if request == nil {
		return grant
	}
	grant.WriteFiles = grant.WriteFiles && request.WriteFiles
	grant.Execute = grant.Execute && request.Execute
	grant.Network = grant.Network && request.Network
	grant.Tools = slices.DeleteFunc(grant.Tools, func(name string) bool { return !slices.Contains(request.Tools, name) })
	return grant
}

// roleTools names tools only one role may hold, whatever the service grant says.
var roleTools = map[string]string{"set_status": "chief_of_staff", "answer": "chief_of_staff", "escalate": "chief_of_staff", "relay_ruling": "chief_of_staff", "route_amendment": "chief_of_staff", "propose_charter": "chief_of_staff"}

// deniedTools names tools one role may never hold, whatever the service grant says.
var deniedTools = map[string]string{"ask": "chief_of_staff"}

func roleGrant(role string, grant coreadapter.Capabilities) (coreadapter.Capabilities, error) {
	switch role {
	case "mason", "librarian":
	case "chief_of_staff", "architect", "committee", "reviewer", "foreman":
		grant.WriteFiles, grant.Execute, grant.Network = false, false, false
	default:
		return coreadapter.Capabilities{}, errors.New("unknown Osmia role")
	}
	grant.Tools = slices.DeleteFunc(slices.Clone(grant.Tools), func(name string) bool {
		owner, reserved := roleTools[name]
		return reserved && owner != role || deniedTools[name] == role
	})
	return grant, nil
}

func (r *Turns) Run(ctx context.Context, input coreadapter.PreparedTurn) (result coreadapter.SessionResult, err error) {
	result.Session.Backend, result.SessionDirectory = input.Profile.Backend, input.SessionDirectory
	defer func() {
		if result.Session.ID == "" {
			result.Session = coreadapter.BackendSession{}
		}
		if err != nil {
			result.IsError = true
			if result.ErrorSubtype == "" {
				result.ErrorSubtype = "isolation"
			}
		}
	}()
	if r.Select == nil || r.Workspaces == nil {
		return result, errors.New("service workspace selection is required")
	}
	if !reflect.DeepEqual(input.Sandbox, coreadapter.SandboxLease{}) || len(input.MCP) != 0 || input.WorkspaceLease != nil || len(input.Cleanup) != 0 || !reflect.DeepEqual(input.Execution, coreadapter.ExecutionSettings{}) {
		return result, errors.New("turn cannot supply workspace, sandbox, MCP or execution overrides")
	}
	grant, ok := r.Grants[input.Scope.Role]
	if !ok {
		return result, errors.New("role has no service grant")
	}
	grant, err = roleGrant(input.Scope.Role, grant)
	if err != nil {
		return result, err
	}
	selected, err := r.Select(ctx, input.Scope)
	if err != nil {
		return result, err
	}
	capabilities := narrow(grant, selected.Narrow)
	selected.Workspace.Scope = input.Scope
	selected.Workspace.Access = coreadapter.ReadOnly
	if capabilities.WriteFiles {
		selected.Workspace.Access = coreadapter.ReadWrite
	}
	// Caller maps are cloned before inserting generated credentials.
	env := maps.Clone(selected.Environment)
	if env == nil {
		env = map[string]string{}
	}
	if _, exists := env["OSMIA_MCP_TOKEN"]; exists {
		return result, errors.New("MCP credentials must be service-generated")
	}
	if err = coreadapter.PublicEnvironment(env); err != nil {
		return result, err
	}
	workspace, err := r.Workspaces.Acquire(ctx, selected.Workspace)
	if err != nil {
		return result, err
	}
	if workspace.Lease == nil {
		return result, errors.New("provider returned no service-owned lease")
	}
	defer func() { err = errors.Join(err, workspace.Lease.Release(context.WithoutCancel(ctx))) }()
	if workspace.Workspace.Access != selected.Workspace.Access {
		return result, errors.New("provider workspace access differs from service selection")
	}
	view, err := r.Views.Create(ctx, workspace.Workspace, selected.Paths)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, view.Release(context.WithoutCancel(ctx))) }()
	tools := append(fileTools(view), r.Tools...)
	if r.Scoped != nil {
		scoped, scopedErr := r.Scoped(ctx, input.Scope)
		if scopedErr != nil {
			return result, scopedErr
		}
		tools = append(tools, scoped...)
	}
	registry := map[string]coreadapter.Tool{}
	for _, tool := range tools {
		if _, exists := registry[tool.Name]; exists {
			return result, errors.New("duplicate service tool")
		}
		registry[tool.Name] = tool
	}
	var approved []coreadapter.Tool
	var names []string
	for _, name := range capabilities.Tools {
		tool, exists := registry[name]
		if !exists {
			return result, fmt.Errorf("granted tool %q is not registered by the service", name)
		}
		if coreadapter.ToolPermitted(capabilities, tool) && !slices.Contains(names, name) {
			approved = append(approved, tool)
			names = append(names, name)
		}
	}
	capabilities.Tools = names
	prepared := input
	prepared.Execution = selected.Execution
	prepared.Sandbox = coreadapter.SandboxLease{Verified: coreadapter.Isolation{Workspace: view.Workspace(), Capabilities: capabilities, Environment: env,
		DenyVCS: true, DenyInheritedEnvironment: true, DenyDeliveryCredentials: true}}
	executor := coreadapter.CoreExecutor{Required: prepared.Sandbox.Verified, Runner: r.Engine}
	if err = executor.Check(ctx, prepared.Sandbox.Verified, prepared.Execution); err != nil {
		return result, err
	}
	if len(approved) != 0 {
		if r.Hosts == nil {
			return result, errors.New("service MCP host is required for granted tools")
		}
		token := rand.Text()
		host := r.Hosts(token)
		if host == nil {
			return result, errors.New("service MCP host is missing")
		}
		hosted, hostErr := host.Host(ctx, coreadapter.HostRequest{Scope: input.Scope, Capabilities: capabilities, Tools: approved})
		if hostErr != nil {
			return result, hostErr
		}
		if hosted.Lease == nil {
			return result, errors.New("service MCP host returned no lease")
		}
		defer func() { err = errors.Join(err, hosted.Lease.Release(context.WithoutCancel(ctx))) }()
		if hosted.Endpoint.BearerTokenEnvironment != "OSMIA_MCP_TOKEN" {
			return result, errors.New("MCP host must use the service token environment")
		}
		env["OSMIA_MCP_TOKEN"] = token
		prepared.MCP = []coreadapter.Endpoint{hosted.Endpoint}
	}
	result, err = (&coreadapter.TurnRunner{Executor: executor}).Run(ctx, prepared)
	if r.Capture != nil {
		err = errors.Join(err, r.Capture(context.WithoutCancel(ctx), input.Scope, view, result))
	}
	return result, err
}

func fileTools(view *FileView) []coreadapter.Tool {
	return []coreadapter.Tool{
		{Name: "file_read", Description: "Read a file in this turn's view", Effect: coreadapter.ToolRead,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
			Handle: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var input struct{ Path string }
				if err := json.Unmarshal(raw, &input); err != nil {
					return nil, err
				}
				data, err := view.Read(input.Path)
				if err != nil {
					return nil, err
				}
				return json.Marshal(string(data))
			}},
		{Name: "file_write", Description: "Write a file in this turn's view", Effect: coreadapter.ToolWrite,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`),
			Handle: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var input struct{ Path, Content string }
				if err := json.Unmarshal(raw, &input); err != nil {
					return nil, err
				}
				if err := view.Write(input.Path, []byte(input.Content)); err != nil {
					return nil, err
				}
				return json.RawMessage(`{}`), nil
			}},
	}
}
