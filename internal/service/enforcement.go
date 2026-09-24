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

// chiefGrant is what a chief-of-staff thread turn may call: its status,
// priority, amendment decision and question tools. The chief of staff reads its context from the
// prompt.
var chiefGrant = coreadapter.Capabilities{Tools: append([]string{status.ToolName, prioritiseTool, decideAmendmentTool}, questions.ChiefTools...)}

// masonGrant is what a mason thread turn may do: read, write and execute in
// its view of its unit's workspace, ask the chief of staff, file an amendment
// and report its unit done.
var masonGrant = coreadapter.Capabilities{Tools: []string{"file_read", "file_write", questions.AskTool, questions.AmendTool, doneTool}, WriteFiles: true, Execute: true}
var reviewerGrant = coreadapter.Capabilities{Tools: []string{"file_read", questions.AskTool, questions.AmendTool, verdictTool}}

// Enforce returns opts with Librarian, Architect, Committee and Threads
// running every role turn through e. Thread turns are granted to the chief of
// staff, mason and reviewer; a turn of any other role fails with a recorded
// reason. A mason turn works on a view of its unit's workspace, copied back
// into the workspace after the turn, and a mason turn whose done the service
// accepted ends with the outcome done, the mason's report and its card. Each role's
// sandbox comes from its configuration, and a sandbox the platform cannot
// enforce fails the turn with core's reason. Thread turns take their role's sandbox and the root from the
// configuration the service has loaded, and record UTC times.
func Enforce(opts Options, e Enforcement) Options {
	opts.Librarian = &Librarian{Engine: e.Engine, Hosts: e.Hosts}
	opts.Architect = &Architect{Engine: e.Engine, Hosts: e.Hosts}
	opts.Committee = &Committee{Engine: e.Engine, Hosts: e.Hosts}
	clock := opts.Reconciliation.Now
	if clock == nil {
		clock = time.Now
	}
	now := func() time.Time { return clock().UTC() }
	if opts.controls == nil {
		opts.controls = &runtimeControls{}
	}
	controls := opts.controls
	opts.Threads = func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		root := cfg.Root.String()
		views := filepath.Join(root, "views")
		if err := os.MkdirAll(views, 0700); err != nil {
			return nil, err
		}
		project := string(r.Project())
		units := newUnitWorkspaces(cfg)
		reports := &masonReports{}
		verdicts := &reviewerReports{}
		turns := &isolation.Turns{
			Workspaces:         threadWorkspaces{units: units},
			Views:              isolation.Views{Directory: views},
			PreserveMasonViews: true,
			Grants:             map[string]coreadapter.Capabilities{trace.ChiefOfStaff: chiefGrant, masonRole: masonGrant, reviewerRole: reviewerGrant, "classifier": {}},
			Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
				if scope.Role == "classifier" && scope.Project == project {
					workspace := filepath.Join(root, "classifier", project)
					if err := os.MkdirAll(workspace, 0700); err != nil {
						return isolation.Selection{}, err
					}
					role := cfg.Roles[masonRole]
					return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Execution: coreadapter.ExecutionSettings{Mode: role.Sandbox, Image: role.Image}}, nil
				}
				role, ok := cfg.Roles[scope.Role]
				if !ok || scope.Project != project {
					return isolation.Selection{}, errors.New("view selection denied")
				}
				execution := coreadapter.ExecutionSettings{Mode: role.Sandbox, Image: role.Image}
				if scope.Role == masonRole {
					return units.selection(ctx, scope, execution)
				}
				// The chief of staff is handed an empty workspace.
				workspace := filepath.Join(root, "workspaces", project, scope.Workstream)
				if err := os.MkdirAll(workspace, 0700); err != nil {
					return isolation.Selection{}, err
				}
				return isolation.Selection{
					Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace},
					Execution: execution,
				}, nil
			},
			Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				if scope.Role == masonRole {
					ask, err := questions.Tools(r, masonAgent(scope.Unit), scope, now)
					return append(ask, reports.tool(r, scope)), err
				}
				if scope.Role == reviewerRole {
					ask, err := questions.Tools(r, reviewerAgent(scope.Unit), scope, now)
					return append(ask, verdicts.tool(scope)), err
				}
				if scope.Role != trace.ChiefOfStaff {
					return nil, nil
				}
				set, err := status.Tool(r, trace.ChiefOfStaff, scope, now)
				if err != nil {
					return nil, err
				}
				chief, err := questions.Tools(r, trace.ChiefOfStaff, scope, now)
				return append([]coreadapter.Tool{set, controls.prioritise(r, scope, now), controls.decideAmendment(r, scope)}, chief...), err
			},
			Hosts:  e.Hosts,
			Engine: e.Engine,
			Capture: func(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView, result coreadapter.SessionResult) error {
				if scope.Role != masonRole {
					return nil
				}
				return units.capture(ctx, scope, view, result)
			},
		}
		runner := thread.Runner{Store: r, Turns: &questions.Turns{Turns: &verdictTurns{Turns: &reportingTurns{Turns: turns, reports: reports}, reports: verdicts}, Repository: r}, Now: now}
		if cfg.Project.Classifier != "" {
			profile, err := cfg.NamedProfile(cfg.Project.Classifier)
			if err != nil {
				return nil, err
			}
			runner.ClassifierProfile = &profile
			runner.Classifier = func(ctx context.Context, input coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
				if err := os.MkdirAll(input.SessionDirectory, 0700); err != nil {
					return coreadapter.SessionResult{}, err
				}
				return turns.Run(ctx, input)
			}
		}
		return thread.Dispatcher{Runner: runner,
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				directory := filepath.Join(root, "threads", project, string(in.Workstream), in.Agent, in.Turn)
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			}}, nil
	}
	return opts
}

// threadWorkspaces lends a mason turn its unit's workspace and every other
// thread turn the directory its selection stages.
type threadWorkspaces struct{ units unitWorkspaces }

func (w threadWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if req.Scope.Role == masonRole {
		return w.units.Acquire(ctx, req)
	}
	return stagedWorkspaces{}.Acquire(ctx, req)
}
