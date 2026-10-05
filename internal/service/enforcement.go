package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/service/beekeeper"
	"github.com/kpenfound/osmia/internal/skills"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// Enforcement is what a service runs its role turns through: the engine
// handing out each role's core enforcer and the host serving each turn's
// scoped tools, and what it runs candidates' checks with. Skills is the
// engine's skill cache, refreshed as the loaded configuration says.
type Enforcement struct {
	Engine coreadapter.Engine
	Hosts  coreadapter.MCPHosts
	Checks ReviewChecks
	Skills *skills.Manager
}

// CoreEnforcement is the production Enforcement for root: core's enforcers
// run real agent sessions with the skills cached in root's skills directory,
// a host turn reaches its tools on a fresh loopback port and a container turn
// reaches them where a container reaches the host.
func CoreEnforcement(root config.Root) Enforcement {
	cache := skills.NewManager(filepath.Join(root.String(), "skills"))
	return Enforcement{
		Engine: coreadapter.CoreEngine{Runner: agent.Runner{Skills: cache, SkillMountDirs: []string{cache.Dir}}},
		Skills: cache,
		Checks: DaggerChecks{},
		Hosts:  &coreadapter.MCPHost{Transport: coreadapter.CoreTransport{}, Container: coreadapter.ContainerTransport(agent.ContainerEngine), Sbx: coreadapter.SbxTransport()},
	}
}

// chiefGrant is what a chief-of-staff thread turn may call: file_read over
// its read-only view of the workstream's documents, status, runtime controls,
// owner decisions, unit moves, drift handbacks and question tools. It names
// every tool chiefTools makes.
var chiefGrant = coreadapter.Capabilities{Tools: append([]string{"file_read", status.ToolName, "notify", "capacity", "inspect_code", prioritiseTool, pauseTool, resumeTool, decideAmendmentTool, decideCharterTool, resolveContestedTool, moveUnitTool, handBackDriftTool}, questions.ChiefTools...)}

// chiefTools are the service tools of one chief-of-staff turn.
func chiefTools(cfg *config.Config, r *trace.Repository, controls *runtimeControls, scope coreadapter.Scope, now func() time.Time) ([]coreadapter.Tool, error) {
	set, err := status.Tool(r, trace.ChiefOfStaff, scope, now)
	if err != nil {
		return nil, err
	}
	chief, err := questions.Tools(r, trace.ChiefOfStaff, scope, now)
	return append([]coreadapter.Tool{set, r.NotifyTool(scope, now), inspectCode(cfg, r, scope, now), controls.capacity(r, scope), controls.prioritise(r, scope, now), controls.pauseControl(r, scope, now, false), controls.pauseControl(r, scope, now, true), controls.decideAmendment(r, scope), controls.decideCharter(r, scope), controls.resolveContested(r, scope), controls.moveUnit(r, scope), controls.handBackDrift(r, scope, now)}, chief...), err
}

// masonGrant is what a mason thread turn may do: read, write and execute in
// its view, ask the chief of staff, file an amendment and report done. A unit
// mason is narrowed to unitMasonGrant; the drift mason to the file tools,
// amend and done.
var masonGrant = coreadapter.Capabilities{Tools: []string{"file_read", "file_write", questions.AskTool, questions.AmendTool, doneTool}, WriteFiles: true, Execute: true}

// unitMasonGrant is what a unit mason turn may do: read, write and execute in
// its view of its unit's workspace, ask the chief of staff and report its
// unit done.
var unitMasonGrant = coreadapter.Capabilities{Tools: []string{"file_read", "file_write", questions.AskTool, doneTool}, WriteFiles: true, Execute: true}

// reviewerGrant is what a unit reviewer turn may do: read its candidate and
// diff, ask the chief of staff and record its verdict. The service runs the
// candidate's checks before review.
var reviewerGrant = coreadapter.Capabilities{Tools: []string{"file_read", questions.AskTool, verdictTool, workstreamDiffTool}}

// threadExecution is how a thread turn of role runs: in the role's sandbox
// and image with the role's skills, and for a mason with the Dagger engine the
// role configures.
func threadExecution(role string, r config.Role) coreadapter.ExecutionSettings {
	execution := coreadapter.ExecutionSettings{Mode: r.Sandbox, Image: r.Image, Skills: slices.Clone(r.Skills)}
	if role == masonRole {
		execution.Dagger = r.Dagger.Settings()
	}
	return execution
}

// Enforce returns opts with Librarian, Architect, Committee and Threads
// running every role turn through e. Thread turns are granted to the chief of
// staff, mason and reviewer; a turn of any other role fails with a recorded
// reason. A chief-of-staff turn reads a view of its workstream's documents,
// staged afresh for each turn. A mason turn works on a view of its unit's workspace, copied back
// into the workspace after the turn, and a mason turn whose done the service
// accepted ends with the outcome done, the mason's report and its card. A
// drift mason turn works the same way on its workstream's drift resolution
// workspace, with file tools, amend and done alone, and a drift reviewer
// turn holds file_read and verdict alone. An amendment a drift mason files,
// or a unit reviewer files while it reads a candidate a drift rebase
// carried, cites that drift rebase's upstream commit. Each role's
// sandbox comes from its configuration, and a sandbox the platform cannot
// enforce fails the turn with core's reason. Thread turns take their role's sandbox and the root from the
// configuration the service has loaded, and record UTC times. Unit and final
// review check runs use e.Checks, or the runner opts already holds when e has
// none.
func Enforce(opts Options, e Enforcement) Options {
	if e.Checks != nil {
		opts.reviewChecks = e.Checks
	}
	opts.skills = e.Skills
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
	opts.BeekeeperTurns = beekeeperTurns(opts, e, controls)
	opts.Threads = func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		root := cfg.Root.String()
		views := filepath.Join(root, "views")
		if err := os.MkdirAll(views, 0700); err != nil {
			return nil, err
		}
		project := string(r.Project())
		units := newUnitWorkspaces(cfg, r)
		drifts := resolutions{driftWorkspaces(cfg, r)}
		reports := &masonReports{}
		verdicts := &reviewerReports{}
		turns := &isolation.Turns{
			Workspaces:         threadWorkspaces{units: units, drifts: drifts},
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
				execution := threadExecution(scope.Role, role)
				if scope.Role == masonRole && scope.Thread == driftMasonAgent {
					return drifts.selection(ctx, scope, execution)
				}
				if scope.Role == masonRole {
					return units.selection(ctx, scope, execution)
				}
				if scope.Role == reviewerRole && scope.Thread != driftReviewerAgent {
					return unitReviewerSelection(ctx, cfg, r, scope, execution)
				}
				if scope.Role == trace.ChiefOfStaff {
					stream, err := config.ParseWorkstreamID(scope.Workstream)
					if err != nil {
						return isolation.Selection{}, err
					}
					workspace := filepath.Join(root, "chief_of_staff", project, scope.Workstream)
					paths, err := stageChiefDocuments(r, stream, workspace)
					if err != nil {
						return isolation.Selection{}, err
					}
					return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: execution}, nil
				}
				// A drift reviewer is handed an empty workspace; it may only
				// read it and record its verdict.
				workspace := filepath.Join(root, "workspaces", project, scope.Workstream)
				if err := os.MkdirAll(workspace, 0700); err != nil {
					return isolation.Selection{}, err
				}
				selection := isolation.Selection{
					Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace},
					Execution: execution,
				}
				if scope.Role == reviewerRole && scope.Thread == driftReviewerAgent {
					selection.Narrow = &coreadapter.Capabilities{Tools: []string{"file_read", verdictTool, workstreamDiffTool}}
				}
				return selection, nil
			},
			Scoped: func(ctx context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				if scope.Role == masonRole && scope.Thread == driftMasonAgent {
					amend, err := driftMasonTools(r, scope, now)
					return append([]coreadapter.Tool{reports.driftTool(scope)}, amend...), err
				}
				if scope.Role == reviewerRole && scope.Thread == driftReviewerAgent {
					diff, err := driftDiffTool(ctx, cfg, r, scope)
					return []coreadapter.Tool{verdicts.tool(scope), diff}, err
				}
				if scope.Role == masonRole {
					ask, err := questions.Tools(r, masonAgent(scope.Unit), scope, now)
					return append(ask, reports.tool(r, scope)), err
				}
				if scope.Role == reviewerRole {
					ask, err := questions.Tools(r, reviewerAgent(scope.Unit), scope, now)
					identity, identityErr := unitReviewerIdentity(r, scope)
					if identityErr != nil {
						return nil, identityErr
					}
					return append(ask, verdicts.tool(scope), reviewDiffTool(cfg, r, scope, identity)), err
				}
				if scope.Role != trace.ChiefOfStaff {
					return nil, nil
				}
				return chiefTools(cfg, r, controls, scope, now)
			},
			Hosts:  e.Hosts,
			Engine: e.Engine,
			Capture: func(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView, result coreadapter.SessionResult) error {
				if scope.Role != masonRole {
					return nil
				}
				if scope.Thread == driftMasonAgent {
					return drifts.capture(ctx, scope, view)
				}
				return units.capture(ctx, scope, view, result)
			},
		}
		turns = memoryTurns(turns, cfg, r)
		runner := threadRunner(cfg, r, &questions.Turns{Turns: &verdictTurns{Turns: &reportingTurns{Turns: &retriedTurns{Turns: turns, units: units, repository: r}, reports: reports}, reports: verdicts}, Repository: r}, now)
		if service := controls.service.Load(); service != nil {
			runner.OnProviderLimit = service.recordProviderLimit
			runner.AdmitRole = service.admitRole
			runner.Jev = service.jev
			runner.Advise = service.questionSignals(r)
		}
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
			},
			Relay: func(ctx context.Context, t trace.Thread, q trace.QueuedTurn) error {
				if service := controls.service.Load(); service != nil {
					return service.relayBeekeeperReply(ctx, t, q)
				}
				return nil
			}}, nil
	}
	return opts
}

// threadWorkspaces lends a drift mason turn its workstream's resolution
// workspace, every other mason turn its unit's workspace and every other
// thread turn the directory its selection stages.
type threadWorkspaces struct {
	units  unitWorkspaces
	drifts resolutions
}

func (w threadWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if req.Scope.Role == masonRole && req.Scope.Thread == driftMasonAgent {
		return w.drifts.Acquire(ctx, req)
	}
	if req.Scope.Role == masonRole {
		return w.units.Acquire(ctx, req)
	}
	return stagedWorkspaces{}.Acquire(ctx, req)
}

// driftMasonTools returns the question tools of a drift mason turn: amend,
// citing the drift rebase whose conflicts it resolves, or none while no
// resolution is in progress.
func driftMasonTools(r *trace.Repository, scope coreadapter.Scope, now func() time.Time) ([]coreadapter.Tool, error) {
	move, found, err := resolvingMove(r, config.WorkstreamID(scope.Workstream))
	if err != nil || !found {
		return nil, err
	}
	return questions.DriftTools(r, driftMasonAgent, scope, now, move)
}

// beekeeperTurns is the execution boundary of every Beekeeper turn: a
// view of its own empty workspace, with no file_read or file_write grant,
// and exactly two tools, list_factory and message_chief_of_staff, both
// bound to controls. Its runtime session settings come only from the
// Beekeeper section of the loaded configuration, read afresh for each
// turn; no registered project's configuration, charter or capacity limits
// ever apply to it.
func beekeeperTurns(opts Options, e Enforcement, controls *runtimeControls) *isolation.Turns {
	root := opts.Config.Root
	views := filepath.Join(root, "views")
	workspace := filepath.Join(root, "beekeeper", "workspace")
	return &isolation.Turns{
		Workspaces: stagedWorkspaces{},
		Views:      isolation.Views{Directory: views},
		Grants:     map[string]coreadapter.Capabilities{beekeeper.AgentID: {Tools: []string{listFactoryTool, messageChiefOfStaffTool}}},
		Select: func(_ context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			if scope.Role != beekeeper.AgentID {
				return isolation.Selection{}, errors.New("view selection denied")
			}
			service := controls.service.Load()
			if service == nil {
				return isolation.Selection{}, errors.New("the service is not ready")
			}
			b := service.current().Beekeeper
			if err := os.MkdirAll(views, 0700); err != nil {
				return isolation.Selection{}, err
			}
			if err := os.MkdirAll(workspace, 0700); err != nil {
				return isolation.Selection{}, err
			}
			return isolation.Selection{
				Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace},
				Execution: coreadapter.ExecutionSettings{Mode: b.Sandbox, Image: b.Image},
			}, nil
		},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Role != beekeeper.AgentID {
				return nil, errors.New("turn scope denied")
			}
			return []coreadapter.Tool{controls.listFactory(scope), controls.messageChiefOfStaff(scope)}, nil
		},
		Hosts:  e.Hosts,
		Engine: e.Engine,
	}
}
