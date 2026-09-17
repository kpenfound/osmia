package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// Enforcement is what a service runs its role turns through: the engine
// handing out each role's core enforcer and the host serving each turn's
// scoped tools.
type Enforcement struct {
	Engine coreadapter.Engine
	Hosts  coreadapter.MCPHosts
}

// CoreEnforcement is the production Enforcement: core's enforcers run real
// agent sessions, a host turn reaches its tools on a fresh loopback port and
// a container turn reaches them where a container reaches the host.
func CoreEnforcement() Enforcement {
	return Enforcement{
		Engine: coreadapter.CoreEngine{},
		Hosts:  &coreadapter.MCPHost{Transport: coreadapter.CoreTransport{}, Container: coreadapter.ContainerTransport(agent.ContainerEngine)},
	}
}

// chiefGrant is what a chief-of-staff thread turn may call: its status and
// question tools. The chief of staff reads its context from the prompt.
var chiefGrant = coreadapter.Capabilities{Tools: append([]string{status.ToolName}, questions.ChiefTools...)}

// Enforce returns opts with Librarian, Architect and Threads running every
// role turn through e. Thread turns are granted to the chief of staff only;
// a turn of any other role fails with a recorded reason. Each role's sandbox
// comes from its configuration, and a sandbox the platform cannot enforce
// fails the turn with core's reason.
func Enforce(opts Options, e Enforcement) Options {
	opts.Librarian = &Librarian{Engine: e.Engine, Hosts: e.Hosts}
	opts.Architect = &Architect{Engine: e.Engine, Hosts: e.Hosts}
	now := opts.Reconciliation.Now
	if now == nil {
		now = time.Now
	}
	opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		cfg, err := config.Load(opts.Config)
		if err != nil {
			return nil, err
		}
		root := cfg.Root.String()
		views := filepath.Join(root, "views")
		if err := os.MkdirAll(views, 0700); err != nil {
			return nil, err
		}
		project := string(r.Project())
		turns := &isolation.Turns{
			Workspaces: stagedWorkspaces{},
			Views:      isolation.Views{Directory: views},
			Grants:     map[string]coreadapter.Capabilities{trace.ChiefOfStaff: chiefGrant},
			Select: func(_ context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
				role, ok := cfg.Roles[scope.Role]
				if !ok || scope.Project != project {
					return isolation.Selection{}, errors.New("view selection denied")
				}
				// The chief of staff is handed an empty workspace.
				workspace := filepath.Join(root, "workspaces", project, scope.Workstream)
				if err := os.MkdirAll(workspace, 0700); err != nil {
					return isolation.Selection{}, err
				}
				return isolation.Selection{
					Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace},
					Execution: coreadapter.ExecutionSettings{Mode: role.Sandbox, Image: role.Image},
				}, nil
			},
			Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				set, err := status.Tool(r, trace.ChiefOfStaff, scope, now)
				if err != nil {
					return nil, err
				}
				chief, err := questions.Tools(r, trace.ChiefOfStaff, scope, now)
				return append([]coreadapter.Tool{set}, chief...), err
			},
			Hosts:  e.Hosts,
			Engine: e.Engine,
		}
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: &questions.Turns{Turns: turns, Repository: r}, Now: now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				directory := filepath.Join(root, "threads", project, string(in.Workstream), in.Agent, in.Turn)
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			}}, nil
	}
	return opts
}
