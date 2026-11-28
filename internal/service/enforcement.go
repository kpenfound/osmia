package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/clientturns"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	kindregistry "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/questions"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/skills"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verifytool"
)

// Enforcement is what a service runs its role turns through: the engine that
// hands out each role's core enforcer, the host that serves each turn's scoped
// tools, the runner for candidates' checks and the cloud runner those checks
// fall back to when check_fallback is on. Skills is the engine's skill cache,
// refreshed as the loaded configuration says.
type Enforcement struct {
	Engine      coreadapter.Engine
	Hosts       coreadapter.MCPHosts
	Checks      ReviewChecks
	CloudChecks ReviewChecks
	Skills      *skills.Manager
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
		Hosts:  &coreadapter.MCPHost{Transport: coreadapter.CoreTransport{}, Container: coreadapter.ContainerTransport(agent.ContainerEngine), Sbx: coreadapter.SbxTransport()},
	}
}

var managerGrant = coreadapter.Capabilities{Web: true, Tools: append(append([]string{"file_read", factoryContextTool, discoveryTool, readRemoteFileTool, checks.TraceToolName, status.ToolName, "notify", "capacity", "inspect_code", "preview", briefToolName, checksTool, delivery.ChecksChoiceToolName, prioritiseTool, pauseTool, resumeTool, requestChangeTool, decideChangeTool, decideAmendmentTool, intake.DecideCharterTool, resolveContestedTool, resolveObjectionTool, decideTool, flakeFixToolName, triageTool, moveUnitTool, climbSteerTool, climbConcludeTool, moveWorkstreamTool, handBackDriftTool, dropDependencyTool, retryChangeTool, followUpTool, draftBriefToolName, designReadRepoToolName, designTitleToolName}, questions.ManagerTools...), ProjectContextTool)}

var ManagerOrdinaryTools = slices.DeleteFunc(slices.Clone(managerGrant.Tools), func(name string) bool {
	return name == draftBriefToolName || name == designReadRepoToolName || name == designTitleToolName
})

var managerOrdinaryNarrow = coreadapter.Capabilities{Tools: ManagerOrdinaryTools, Web: true}

// ManagerOrdinaryToolsOf lists the tools an ordinary manager turn holds
// for a workstream of the named kind: every ordinary tool except the ones only
// another kind adds. A kind this build does not register holds none of the
// kind tools.
func ManagerOrdinaryToolsOf(kind string) []string {
	own := map[string]bool{}
	if k, ok := kindregistry.Lookup(kind); ok {
		for _, name := range k.ManagerTools {
			own[name] = true
		}
	}
	others := kindregistry.ManagerToolNames()
	return slices.DeleteFunc(slices.Clone(ManagerOrdinaryTools), func(name string) bool {
		return slices.Contains(others, name) && !own[name]
	})
}

var designOnlyTools = []string{"file_read", draftBriefToolName, designReadRepoToolName, designTitleToolName}

var designNarrow = coreadapter.Capabilities{Tools: designOnlyTools, Web: true}

// designerGrant is everything a design turn is granted: the design tools and
// web access. A design session's own agent never holds a write, execution,
// version-control, credential or owner-gate tool, nor any other tool of the
// manager.
var designerGrant = coreadapter.Capabilities{Tools: slices.Clone(designOnlyTools), Web: true}

func managerTools(cfg *config.Config, r *trace.Repository, controls *runtimeControls, scope coreadapter.Scope, now func() time.Time) ([]coreadapter.Tool, error) {
	set, err := status.ToolHolding(r, trace.Manager, scope, now, controls.heldGates(r, config.WorkstreamID(scope.Workstream)))
	if err != nil {
		return nil, err
	}
	kindTools, kindErr := controls.kindManagerTools(r, scope, now)
	if kindErr != nil {
		return nil, kindErr
	}
	manager, err := questions.Tools(r, trace.Manager, scope, now)
	for i, t := range manager {
		if t.Name == questions.RouteAmendmentTool {
			manager[i] = controls.lifecycle().ReopenForRevision(r, scope, t)
		}
	}
	return controls.reportQueued(cfg, r, scope, now, append(append([]coreadapter.Tool{recordDiscovery(r, scope, now), factoryContext(r, scope), remoteFileTool(r, scope, now), controls.cloudTrace(cfg, r, scope, now), set, r.NotifyTool(scope, now), inspectCode(cfg, r, scope, now), controls.capacity(r, scope), controls.deliveryBrief(r, scope), delivery.ChecksToolOf(controls.deliveryController, r, scope), delivery.ChecksChoiceToolOf(controls.deliveryController, r, scope), controls.prioritise(r, scope, now), controls.pauseControl(r, scope, now, false), controls.pauseControl(r, scope, now, true), controls.ChangeControl(r, scope, false), controls.ChangeControl(r, scope, true), controls.decideAmendment(r, scope), controls.decideCharter(r, scope), controls.resolveContested(r, scope), controls.intake().ResolveObjection(r, scope), controls.decide(r, scope), controls.flakeFix(r, scope), controls.triageFinding(r, scope), controls.MoveWorkstream(r, scope), controls.HandBackDrift(r, scope, now), controls.lifecycle().DropDependency(r, scope), controls.lifecycle().RetryChange(r, scope), newFollowUpTool(r, scope, now)}, manager...), append(kindTools, ProjectContext(r, scope))...)), err
}

// kindManagerTools builds the manager tools the workstream's kind adds.
// A workstream whose kind this build does not register gets none.
func (c *runtimeControls) kindManagerTools(r *trace.Repository, scope coreadapter.Scope, now func() time.Time) ([]coreadapter.Tool, error) {
	stream, err := config.ParseWorkstreamID(scope.Workstream)
	if err != nil {
		return nil, err
	}
	kind, _, ok, err := intake.WorkstreamKind(r, stream)
	if err != nil || !ok {
		return nil, err
	}
	builders := map[string]func() coreadapter.Tool{
		moveUnitTool:      func() coreadapter.Tool { return c.moveUnit(r, scope) },
		climbSteerTool:    func() coreadapter.Tool { return c.climbSteer(r, scope, now) },
		climbConcludeTool: func() coreadapter.Tool { return c.climbConclude(r, scope) },
	}
	var tools []coreadapter.Tool
	for _, name := range kind.ManagerTools {
		if build, found := builders[name]; found {
			tools = append(tools, build())
		}
	}
	return tools, nil
}

var masonGrant = coreadapter.Capabilities{Tools: []string{"file_read", "file_write", questions.AskTool, questions.AmendTool, factoryContextTool, discoveryTool, execution.DoneTool}, WriteFiles: true, Execute: true}

var unitMasonGrant = execution.UnitMasonGrant

var reviewerGrant = coreadapter.Capabilities{Tools: []string{"file_read", questions.AskTool, factoryContextTool, discoveryTool, execution.VerdictTool, reviewtools.DiffToolName}}

// Enforce returns opts with Architect, Committee and Threads running every
// role turn through e. Thread turns are granted to the manager, mason
// and reviewer; a turn of any other role fails with a recorded reason. A
// sandbox the platform cannot enforce fails the turn with core's reason. Unit
// and final review check runs use e.Checks, or the runner opts already holds
// when e has none.
func Enforce(opts Options, e Enforcement) Options {
	if e.Checks != nil {
		opts.reviewChecks = e.Checks
	}
	if e.CloudChecks != nil {
		opts.cloudChecks = e.CloudChecks
	}
	opts.skills = e.Skills
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
		units := newUnitWorkspaces(cfg, r)
		drifts := resolutions{StreamWorkspaces: driftWorkspaces(cfg, r)}
		reports := &execution.MasonReports{Verification: func() bool { return controls.currentConfig(r, cfg).Project.Verification.Enabled() }}
		verdicts := &execution.ReviewerReports{}
		turns := &isolation.Turns{
			Workspaces:         threadWorkspaces{units: units, drifts: drifts, sessions: sessionWorkspaces{cfg: cfg}},
			Views:              isolation.Views{Directory: views},
			PreserveMasonViews: true,
			Grants:             map[string]coreadapter.Capabilities{trace.Manager: managerGrant, trace.Designer: designerGrant, masonRole: masonGrant, reviewerRole: reviewerGrant, sessionRole: {WriteFiles: true, Execute: true, Web: true, Tools: []string{"show_image"}}},
			Offered: func(_ context.Context, scope coreadapter.Scope) []string {
				if verifyOffered(controls.currentConfig(r, cfg), scope) {
					return []string{verifytool.ToolName}
				}
				return nil
			},
			Select: func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
				if scope.Role == sessionRole && scope.Project == project {
					return sessionSelection(ctx, cfg, scope, cfg.RoleExecution(masonRole))
				}
				_, ok := cfg.Roles[trace.ConfigRole(scope.Role)]
				if !ok || scope.Project != project {
					return isolation.Selection{}, errors.New("view selection denied")
				}
				settings := cfg.RoleExecution(trace.ConfigRole(scope.Role))
				if scope.Unit != "" && scope.Thread != driftMasonAgent && scope.Thread != driftReviewerAgent {
					if kind, err := r.Kind(config.WorkstreamID(scope.Workstream)); err == nil {
						settings = cfg.WithUnitKindSkills(settings, kind, scope.Role, scope.Unit)
					}
				}
				if scope.Role == masonRole && scope.Thread == driftMasonAgent {
					return drifts.Selection(ctx, scope, settings)
				}
				if scope.Role == masonRole {
					return units.selection(ctx, scope, settings)
				}
				if scope.Role == reviewerRole && scope.Thread == driftReviewerAgent {
					return lifecycle.DriftReviewerSelection(ctx, cfg, r, scope, settings)
				}
				if scope.Role == reviewerRole {
					return execution.UnitReviewerSelection(ctx, cfg, r, scope, settings)
				}
				if scope.Role == trace.Designer {
					stream, err := config.ParseWorkstreamID(scope.Workstream)
					if err != nil {
						return isolation.Selection{}, err
					}
					design, err := isDesignWorkstream(r, stream)
					if err != nil {
						return isolation.Selection{}, err
					}
					if !design {
						return isolation.Selection{}, errors.New("a designer turn runs in an open design session only")
					}
					return designSelection(ctx, controls, r, root, project, stream, scope, settings)
				}
				if scope.Role == trace.Manager {
					stream, err := config.ParseWorkstreamID(scope.Workstream)
					if err != nil {
						return isolation.Selection{}, err
					}
					design, err := isDesignWorkstream(r, stream)
					if err != nil {
						return isolation.Selection{}, err
					}
					if design {
						return designSelection(ctx, controls, r, root, project, stream, scope, settings)
					}
					workspace := filepath.Join(root, trace.Manager, project, scope.Workstream)
					paths, err := StageManagerDocuments(r, stream, workspace)
					if err != nil {
						return isolation.Selection{}, err
					}
					stored, err := r.Kind(stream)
					if err != nil {
						return isolation.Selection{}, err
					}
					tools := ManagerOrdinaryToolsOf(stored)
					if verifyOffered(controls.currentConfig(r, cfg), scope) {
						tools = append(tools, verifytool.ToolName)
					}
					return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings,
						Narrow: &coreadapter.Capabilities{Tools: tools, Web: true}}, nil
				}
				return isolation.Selection{}, errors.New("view selection denied")
			},
			Scoped: func(ctx context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				if scope.Role == masonRole && scope.Thread == driftMasonAgent {
					amend, err := driftMasonTools(r, scope, now)
					return append([]coreadapter.Tool{reports.DriftTool(scope), recordDiscovery(r, scope, now), factoryContext(r, scope)}, amend...), err
				}
				if scope.Role == reviewerRole && scope.Thread == driftReviewerAgent {
					ask, err := questions.Tools(r, driftReviewerAgent, scope, now)
					if err != nil {
						return nil, err
					}
					diff, err := lifecycle.DriftDiffTool(ctx, cfg, r, scope)
					if err != nil {
						return nil, err
					}
					return append(ask, verdicts.Tool(scope), recordDiscovery(r, scope, now), factoryContext(r, scope), diff), nil
				}
				if scope.Role == masonRole && scope.Thread == execution.ClimbMasonAgent {
					ask, err := questions.Tools(r, execution.ClimbMasonAgent, scope, now)
					return append(ask, recordDiscovery(r, scope, now), factoryContext(r, scope), reports.AttemptTool(r, scope)), err
				}
				if scope.Role == masonRole {
					ask, err := questions.Tools(r, masonAgent(scope.Unit), scope, now)
					return append(ask, recordDiscovery(r, scope, now), factoryContext(r, scope), reports.Tool(r, scope)), err
				}
				if scope.Role == reviewerRole {
					ask, err := questions.Tools(r, reviewerAgent(scope.Unit), scope, now)
					identity, identityErr := execution.UnitReviewerIdentity(r, scope)
					if identityErr != nil {
						return nil, identityErr
					}
					return append(ask, recordDiscovery(r, scope, now), factoryContext(r, scope), verdicts.Tool(scope), execution.ReviewDiffTool(cfg, r, scope, identity)), err
				}
				if scope.Role == trace.Designer {
					return []coreadapter.Tool{controls.intake().Load().DraftBriefTool(r, scope, now), intake.DesignReadRepoTool(cfg, r, controls.intake(), scope), intake.DesignTitleTool(r, scope, now)}, nil
				}
				if scope.Role != trace.Manager {
					return nil, nil
				}
				tools, err := managerTools(cfg, r, controls, scope, now)
				if err != nil {
					return nil, err
				}
				return append(tools, controls.intake().Load().DraftBriefTool(r, scope, now), intake.DesignReadRepoTool(cfg, r, controls.intake(), scope), intake.DesignTitleTool(r, scope, now)), nil
			},
			ViewTools: func(ctx context.Context, scope coreadapter.Scope, view isolation.View) ([]coreadapter.Tool, error) {
				if scope.Role == sessionRole && scope.Project == project {
					return []coreadapter.Tool{showImageTool(r, scope, view, now)}, nil
				}
				if scope.Role == trace.Manager {
					tools := []coreadapter.Tool{controls.preview(cfg, r, scope, view, now)}
					if verifyOffered(controls.currentConfig(r, cfg), scope) {
						tools = append(tools, controls.verify(cfg, r, scope, view, now))
					}
					return tools, nil
				}
				if verifyOffered(controls.currentConfig(r, cfg), scope) {
					return []coreadapter.Tool{controls.verify(cfg, r, scope, view, now)}, nil
				}
				return nil, nil
			},
			Seed: func(ctx context.Context, scope coreadapter.Scope, view isolation.View) error {
				if scope.Role == trace.Manager {
					if err := restorePreviewImages(ctx, cfg, controls, r, scope, view, now); err != nil {
						return err
					}
				}
				return controls.seedVerification(ctx, cfg, r, scope, view, now)
			},
			Hosts:  e.Hosts,
			Engine: resumableEngine{e.Engine},
			Capture: func(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView, result coreadapter.SessionResult) error {
				if scope.Role == sessionRole {
					return sessionWorkspaces{cfg: cfg}.capture(ctx, scope, view)
				}
				if scope.Role != masonRole {
					return nil
				}
				if scope.Thread == driftMasonAgent {
					return drifts.Capture(ctx, scope, view)
				}
				return units.capture(ctx, scope, view, result)
			},
		}
		if service := controls.service.Load(); service != nil && service.clients != nil {
			turns.Clients = service.clients.Placer()
		}
		turns.Chain = func(profile string) []string { return fleet.Fallbacks(cfg, profile) }
		turns = memoryTurns(turns, cfg, r)
		runner := threadRunner(cfg, r, &questions.Turns{Turns: execution.NewVerdictTurns(execution.NewReportingTurns(execution.NewRetriedTurns(turns, units.UnitWorkspaces, r), reports), verdicts), Repository: r}, now)
		if service := controls.service.Load(); service != nil {
			runner.OnProviderLimit = service.recordProviderLimit
			runner.AdmitRole = service.admitRole
			runner.Jev = service.jev
			if service.clients != nil {
				service.clients.Bind(r.Project(), clientturns.Binding{Repository: r, Runner: runner, Capture: turns.Capture, Rehost: turns.Rehost})
			}
		}
		runner.ResumableRoles = resumableRole
		runner.LegacyBaseline = legacyBaseline(turns.Grants)
		if service := controls.service.Load(); service != nil && service.placement != nil {
			runner.Place = service.placeTurn(r)
		}
		return thread.Dispatcher{Runner: runner,
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				directory := filepath.Join(root, "threads", project, string(in.Workstream), in.Agent, in.Turn)
				if in.Agent == SessionAgentID {
					directory = filepath.Join(runtime.ModelSessionDirectory(root, r.Project(), in.Workstream), in.Turn)
				}
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			},
			Relay: func(ctx context.Context, t trace.Thread, q trace.QueuedTurn) error {
				if t.Identity.Role == sessionRole {
					return SessionRelay(ctx, r, now, t, q)
				}
				return nil
			}}, nil
	}
	return opts
}

// designSelection is the view and the narrowing of a design turn: the staged
// reference documents of the projects outside the brief, and exactly the
// design tools.
func designSelection(ctx context.Context, controls *runtimeControls, r *trace.Repository, root, project string, stream config.WorkstreamID, scope coreadapter.Scope, settings coreadapter.ExecutionSettings) (isolation.Selection, error) {
	workspace := filepath.Join(root, "design", project, scope.Workstream)
	paths, err := controls.intake().Load().StageDesignWorkspace(ctx, r, stream, workspace)
	if err != nil {
		return isolation.Selection{}, err
	}
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: paths, Execution: settings, Narrow: &designNarrow}, nil
}

type threadWorkspaces struct {
	units    unitWorkspaces
	drifts   resolutions
	sessions sessionWorkspaces
}

func (w threadWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if req.Scope.Role == masonRole && req.Scope.Thread == driftMasonAgent {
		return w.drifts.Acquire(ctx, req)
	}
	if req.Scope.Role == masonRole {
		return w.units.Acquire(ctx, req)
	}
	if req.Scope.Role == sessionRole {
		return w.sessions.Acquire(ctx, req)
	}
	return stagedWorkspaces{}.Acquire(ctx, req)
}

func driftMasonTools(r *trace.Repository, scope coreadapter.Scope, now func() time.Time) ([]coreadapter.Tool, error) {
	move, found, err := lifecycle.ResolvingMove(r, config.WorkstreamID(scope.Workstream))
	if err != nil || !found {
		return nil, err
	}
	return questions.DriftTools(r, driftMasonAgent, scope, now, move)
}
