package coreadapter_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/vcs"
	a "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
)

func TestRawExecutionRequestCannotWidenBoundary(t *testing.T) {
	mutations := map[string]func(*agent.Request){
		"VCS":              func(r *agent.Request) { r.Profile.VCSAccess = true },
		"VCS grant":        func(r *agent.Request) { r.Grants.VCS = true },
		"granted variable": func(r *agent.Request) { r.Grants.Env = append(r.Grants.Env, "GH_TOKEN") },
		"granted tool":     func(r *agent.Request) { r.Grants.Tools = append(r.Grants.Tools, "Bash") },
		"granted mount": func(r *agent.Request) {
			r.Grants.Mounts = append(r.Grants.Mounts, agent.Mount{Path: "/", Access: agent.ReadOnly})
		},
		"writable view":      func(r *agent.Request) { r.Grants.Mounts[0].Access = agent.ReadWrite },
		"profile env":        func(r *agent.Request) { r.Profile.Env = map[string]string{"GH_TOKEN": "secret"} },
		"container env":      func(r *agent.Request) { r.ContainerEnv = map[string]string{"GH_TOKEN": "secret"} },
		"VCS env":            func(r *agent.Request) { r.VCSEnv = map[string]string{"GH_TOKEN": "secret"} },
		"VCS container env":  func(r *agent.Request) { r.VCSContainerEnv = map[string]string{"GH_TOKEN": "secret"} },
		"host MCP":           func(r *agent.Request) { r.HostMCP = &agent.HostMCP{} },
		"skills":             func(r *agent.Request) { r.Profile.Skills = []string{"repo-plugin"} },
		"shell":              func(r *agent.Request) { r.Profile.Shell = "/bin/sh" },
		"container override": func(r *agent.Request) { r.Profile.ContainerUseEnvironment = "other" },
		"tools":              func(r *agent.Request) { r.Profile.AllowedTools = []string{"Bash"} },
		"server-wide tools":  func(r *agent.Request) { r.Profile.AllowedTools = []string{"mcp__osmia_0"} },
		"environment":        func(r *agent.Request) { r.Env["SSH_AUTH_SOCK"] = "/agent" },
		"MCP command":        func(r *agent.Request) { r.Profile.MCP = map[string]agent.MCPEntry{"rogue": {Command: "git"}} },
		"MCP headers": func(r *agent.Request) {
			r.Profile.MCP = map[string]agent.MCPEntry{"rogue": {Type: "http", URL: "http://service", BearerTokenEnv: "OSMIA_MCP_TOKEN", Headers: map[string]string{"Authorization": "secret"}}}
		},
		"MCP URL credentials": func(r *agent.Request) {
			r.Profile.MCP = map[string]agent.MCPEntry{"rogue": {Type: "http", URL: "http://secret@service", BearerTokenEnv: "OSMIA_MCP_TOKEN"}}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			turn := boundaryTurn(t, "container")
			turn.Sandbox.Verified.Environment["OSMIA_MCP_TOKEN"] = "scoped-fixture-token"
			engine := &adaptertest.Engine{}
			executor := a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}
			if _, err := (&a.TurnRunner{Executor: executor}).Run(context.Background(), turn); err != nil {
				t.Fatal(err)
			}
			req := engine.Requests[0]
			req.Workspace = vcs.Directory(turn.Sandbox.Verified.Workspace.Directory)
			if _, err := executor.Run(context.Background(), req, turn.Execution); err != nil {
				t.Fatalf("unchanged request refused: %v", err)
			}
			mutate(&req)
			if _, err := executor.Run(context.Background(), req, turn.Execution); !errors.Is(err, a.ErrUnsupported) {
				t.Fatal(err)
			}
			if len(engine.Requests) != 2 {
				t.Fatal("raw override reached construction")
			}
			// Only core's session can refuse these; every other widening is
			// refused before a session is prepared.
			prepared := 2
			if coreRefuses[name] {
				prepared = 3
			}
			if len(engine.Prepared) != prepared || !engine.Released() {
				t.Fatalf("prepared %d sessions, want %d, all released", len(engine.Prepared), prepared)
			}
		})
	}
}

// coreRefuses names the widening requests that reach core's session.
var coreRefuses = map[string]bool{"VCS": true, "profile env": true, "host MCP": true, "shell": true}

func boundaryTurn(t *testing.T, mode string) a.PreparedTurn {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return a.PreparedTurn{Scope: a.Scope{Role: "committee"}, Profile: a.Profile{Backend: "claude"}, SessionDirectory: t.TempDir(),
		Execution: a.ExecutionSettings{Mode: mode, Image: "fixture-image"},
		Sandbox: a.SandboxLease{Verified: a.Isolation{Workspace: a.Workspace{Directory: dir, Access: a.ReadOnly}, Environment: map[string]string{"LANG": "C"},
			DenyVCS: true, DenyInheritedEnvironment: true, DenyDeliveryCredentials: true}}}
}

func TestContainerConstruction(t *testing.T) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "SSH_AUTH_SOCK", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "HOME", "PATH"} {
		t.Setenv(key, "host-secret")
	}
	turn := boundaryTurn(t, "container")
	engine := &adaptertest.Engine{}
	runner := a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}
	if _, err := runner.Run(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	if len(engine.Prepared) != 1 || len(engine.Requests) != 1 || !engine.Released() {
		t.Fatal("session or execution missing")
	}
	req := engine.Requests[0]
	view := turn.Sandbox.Verified.Workspace.Directory
	scratch := filepath.Join(turn.SessionDirectory, "work")
	want := agent.Grants{Env: []string{"LANG"}, Tools: []string{}, Mounts: []agent.Mount{{Path: view, Access: agent.ReadOnly}, {Path: turn.SessionDirectory, Access: agent.ReadOnly}, {Path: scratch, Access: agent.ReadWrite}}}
	if req.Grants == nil || !reflect.DeepEqual(*req.Grants, want) {
		t.Fatalf("grants: %+v", req.Grants)
	}
	if req.Workspace.Directory() != scratch {
		t.Fatalf("read-only view is the working directory: %s", req.Workspace.Directory())
	}
	if info, err := os.Stat(scratch); err != nil || !info.IsDir() {
		t.Fatalf("scratch directory: %v", err)
	}
	if !reflect.DeepEqual(req.Env, map[string]string{"LANG": "C"}) || req.Profile.VCSAccess || req.Workspace.VCS() != nil || len(req.Profile.MCP) != 0 || len(req.Profile.AllowedTools) != 0 {
		t.Fatalf("ambient access: %+v", req)
	}
	verified, err := (&agent.Runner{}).Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(verified.Env, []string{"LANG=C", "HOME=/home/agent"}) || verified.VCS || len(verified.Tools) != 0 || !reflect.DeepEqual(verified.DeniedExecutables, agent.VCSExecutables) {
		t.Fatalf("turn: %+v", verified)
	}
	for _, bind := range verified.Binds {
		if bind.Access != agent.ReadOnly && bind.Source != scratch {
			t.Fatalf("writable bind: %+v", bind)
		}
	}
}

func TestEverySandboxModeReachesRun(t *testing.T) {
	for _, mode := range []string{"none", "claude", "container"} {
		t.Run(mode, func(t *testing.T) {
			turn := toolTurn(t, mode)
			engine := &adaptertest.Engine{}
			result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine, Pinned: []string{t.TempDir()}}}).Run(context.Background(), turn)
			if err != nil || result.FinalResponse != "fixture response" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if len(engine.Requests) != 1 || !engine.Released() {
				t.Fatalf("launches=%d released=%v", len(engine.Requests), engine.Released())
			}
			req := engine.Requests[0]
			if req.Profile.Sandbox != mode || req.Profile.Confine != (mode != "container") || req.Profile.SandboxImage != turn.Execution.Image {
				t.Fatalf("admitted profile: %+v", req.Profile)
			}
			if req.Grants == nil || !reflect.DeepEqual(req.Grants.Tools, []string{"mcp__osmia_0"}) {
				t.Fatalf("admitted grants: %+v", req.Grants)
			}
		})
	}
}

// toolTurn is a read-only turn in mode with one granted tool on one service
// MCP server.
func toolTurn(t *testing.T, mode string) a.PreparedTurn {
	t.Helper()
	turn := boundaryTurn(t, mode)
	if mode != "container" {
		turn.Execution.Image = ""
	}
	turn.Sandbox.Verified.Capabilities.Tools = []string{"notes_read"}
	turn.Sandbox.Verified.Environment["OSMIA_MCP_TOKEN"] = "scoped-fixture-token"
	turn.MCP = []a.Endpoint{{URL: "http://127.0.0.1:1/mcp", BearerTokenEnvironment: "OSMIA_MCP_TOKEN", Token: "scoped-fixture-token"}}
	return turn
}

func TestPolicyMismatchNeverStarts(t *testing.T) {
	pinned := t.TempDir()
	mutations := map[string]func(*agent.Policy, string){
		"sandbox":         func(p *agent.Policy, _ string) { p.Sandbox = agent.SandboxNone },
		"image":           func(p *agent.Policy, _ string) { p.Image = "other-image" },
		"writable view":   func(p *agent.Policy, view string) { setAccess(p, view, agent.ReadWrite) },
		"unreadable view": func(p *agent.Policy, view string) { p.Denied = append(p.Denied, view) },
		"writable pinned": func(p *agent.Policy, _ string) {
			p.Mounts = append(p.Mounts, agent.Mount{Path: pinned, Access: agent.ReadWrite})
		},
		"VCS":               func(p *agent.Policy, _ string) { p.VCS = true },
		"VCS executable":    func(p *agent.Policy, _ string) { p.DeniedExecutables = p.DeniedExecutables[1:] },
		"every built-in":    func(p *agent.Policy, _ string) { p.Tools = nil },
		"built-in tool":     func(p *agent.Policy, _ string) { p.Tools = []string{"Bash"} },
		"missing server":    func(p *agent.Policy, _ string) { p.MCPServers = nil },
		"additional server": func(p *agent.Policy, _ string) { p.MCPServers = append(p.MCPServers, "rogue") },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			turn := toolTurn(t, "container")
			view := turn.Sandbox.Verified.Workspace.Directory
			engine := &adaptertest.Engine{Policy: func(p *agent.Policy) { mutate(p, view) }}
			result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine, Pinned: []string{pinned}}}).Run(context.Background(), turn)
			var refusal *a.UnsupportedError
			if !errors.As(err, &refusal) || refusal.Capability != "session policy" || !result.IsError {
				t.Fatalf("err=%v result=%+v", err, result)
			}
			if len(engine.Prepared) != 1 || len(engine.Requests) != 0 || !engine.Released() {
				t.Fatalf("prepared=%d launches=%d released=%v", len(engine.Prepared), len(engine.Requests), engine.Released())
			}
		})
	}
}

func setAccess(p *agent.Policy, path string, access agent.Access) {
	for i := range p.Mounts {
		if p.Mounts[i].Path == path {
			p.Mounts[i].Access = access
		}
	}
}

func TestWritableViewPolicyIsAccepted(t *testing.T) {
	turn := toolTurn(t, "none")
	turn.Sandbox.Verified.Workspace.Access = a.ReadWrite
	turn.Sandbox.Verified.Capabilities.WriteFiles = true
	engine := &adaptertest.Engine{}
	executor := a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}
	if _, err := (&a.TurnRunner{Executor: executor}).Run(context.Background(), turn); err != nil || len(engine.Requests) != 1 {
		t.Fatalf("err=%v launches=%d", err, len(engine.Requests))
	}
	// A writable view whose session reads it only is a mismatch too.
	view := turn.Sandbox.Verified.Workspace.Directory
	engine = &adaptertest.Engine{Policy: func(p *agent.Policy) { setAccess(p, view, agent.ReadOnly) }}
	executor.Runner = engine
	if _, err := (&a.TurnRunner{Executor: executor}).Run(context.Background(), turn); !errors.Is(err, a.ErrUnsupported) || len(engine.Requests) != 0 {
		t.Fatalf("err=%v launches=%d", err, len(engine.Requests))
	}
}

func TestChangedPolicyIsARefusal(t *testing.T) {
	turn := boundaryTurn(t, "container")
	engine := &adaptertest.Engine{RunErr: fmt.Errorf("fixture: %w", agent.ErrPolicyChanged)}
	result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
	if !errors.Is(err, a.ErrUnsupported) || !errors.Is(err, agent.ErrPolicyChanged) || !result.IsError {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	if len(engine.Enforcers) != 1 || len(engine.Prepared) != 1 || !engine.Released() {
		t.Fatalf("enforcers=%d prepared=%d released=%v", len(engine.Enforcers), len(engine.Prepared), engine.Released())
	}
}

func TestCoreRefusalNeverStarts(t *testing.T) {
	turn := boundaryTurn(t, "container")
	for _, refusal := range []error{agent.ErrNotGranted, agent.ErrUnsupported, agent.ErrNoGrants} {
		engine := &adaptertest.Engine{PrepareErr: fmt.Errorf("fixture reason: %w", refusal)}
		result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
		if !errors.Is(err, a.ErrUnsupported) || !errors.Is(err, refusal) || !strings.Contains(err.Error(), "fixture reason") || !result.IsError || len(engine.Requests) != 0 {
			t.Fatalf("%v: err=%v launches=%d", refusal, err, len(engine.Requests))
		}
	}
	// Any other failure, such as an unreadable path or an engine that has no
	// enforcer, is not a refusal and keeps its own error.
	failure := errors.New("fixture I/O failure")
	for _, engine := range []*adaptertest.Engine{{PrepareErr: failure}, {EnforcerErr: failure}} {
		_, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
		if errors.Is(err, a.ErrUnsupported) || !errors.Is(err, failure) || len(engine.Requests) != 0 {
			t.Fatalf("err=%v launches=%d", err, len(engine.Requests))
		}
	}
}

func TestNewEnforcer(t *testing.T) {
	runner := agent.Runner{}
	kinds := map[string]string{}
	for _, settings := range []a.ExecutionSettings{{Mode: "none"}, {Mode: "claude"}, {Mode: "container", Image: "fixture-image"}} {
		e, err := a.NewEnforcer(runner, settings)
		if err != nil || e == nil {
			t.Fatalf("%+v: %v", settings, err)
		}
		kinds[settings.Mode] = fmt.Sprintf("%T", e)
		if _, err := (a.CoreEngine{Runner: runner}).Enforcer(settings); err != nil {
			t.Fatalf("engine %+v: %v", settings, err)
		}
	}
	if kinds["none"] == kinds["container"] {
		t.Fatalf("container enforcer is a host enforcer: %v", kinds)
	}
	for _, settings := range []a.ExecutionSettings{{Mode: "none", Image: "fixture-image"}, {Mode: "claude", Image: "fixture-image"}, {Mode: "container"}, {Mode: "vm"}, {}} {
		if e, err := a.NewEnforcer(runner, settings); !errors.Is(err, a.ErrUnsupported) || e != nil {
			t.Fatalf("%+v: %v", settings, err)
		}
	}
}

// Host enforcers are told apart by the policy of a session they prepare
// under a confiner that allows everything and starts nothing.
func TestNewHostEnforcerKinds(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Confiner: allowAll{}, SystemPaths: []agent.Mount{}}
	for _, mode := range []string{"none", "claude"} {
		e, err := a.NewEnforcer(runner, a.ExecutionSettings{Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		session, err := e.Prepare(context.Background(), agent.Grants{Tools: []string{}, Mounts: []agent.Mount{{Path: dir, Access: agent.ReadOnly}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := session.Policy().Sandbox; got != mode {
			t.Fatalf("%s enforcer prepared a %s session", mode, got)
		}
		if err := session.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

type allowAll struct{}

func (allowAll) Check(agent.Confinement) error { return nil }
func (allowAll) Start(cmd *exec.Cmd, _ agent.Confinement) error {
	return errors.New("fixture confiner starts nothing")
}

func TestNonClaudeBackendsAreRefused(t *testing.T) {
	for _, backend := range []string{"codex", "opencode"} {
		t.Run(backend, func(t *testing.T) {
			turn := boundaryTurn(t, "container")
			turn.Profile.Backend = backend
			engine := &adaptertest.Engine{}
			_, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
			if !errors.Is(err, a.ErrUnsupported) || !errors.Is(err, agent.ErrUnsupported) || len(engine.Requests) != 0 {
				t.Fatalf("err=%v launches=%d", err, len(engine.Requests))
			}
		})
	}
}

func TestBoundaryRejectsUnsafeInputs(t *testing.T) {
	mutations := map[string]func(*testing.T, *a.PreparedTurn){
		"extra mount":       func(_ *testing.T, p *a.PreparedTurn) { p.Execution.Mounts = []string{"/host"} },
		"domain override":   func(_ *testing.T, p *a.PreparedTurn) { p.Execution.Domains = []string{"*"} },
		"missing denial":    func(_ *testing.T, p *a.PreparedTurn) { p.Sandbox.Verified.DenyVCS = false },
		"write":             func(_ *testing.T, p *a.PreparedTurn) { p.Sandbox.Verified.Capabilities.WriteFiles = true },
		"run":               func(_ *testing.T, p *a.PreparedTurn) { p.Sandbox.Verified.Capabilities.Execute = true },
		"fetch":             func(_ *testing.T, p *a.PreparedTurn) { p.Sandbox.Verified.Capabilities.Network = true },
		"credentials":       func(_ *testing.T, p *a.PreparedTurn) { p.Sandbox.Verified.Environment["GH_TOKEN"] = "secret" },
		"ssh":               func(_ *testing.T, p *a.PreparedTurn) { p.Sandbox.Verified.Environment["SSH_AUTH_SOCK"] = "/agent" },
		"provider delivery": func(_ *testing.T, p *a.PreparedTurn) { p.Sandbox.Verified.Environment["AWS_ACCESS_KEY_ID"] = "secret" },
		"config home":       func(_ *testing.T, p *a.PreparedTurn) { p.Sandbox.Verified.Environment["HOME"] = "/host" },
		"VCS metadata": func(t *testing.T, p *a.PreparedTurn) {
			if err := os.WriteFile(filepath.Join(p.Sandbox.Verified.Workspace.Directory, ".git"), []byte("gitdir: /private"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"host image": func(_ *testing.T, p *a.PreparedTurn) { p.Execution.Mode = "none" },
		"no image":   func(_ *testing.T, p *a.PreparedTurn) { p.Execution.Image = "" },
		"no mode":    func(_ *testing.T, p *a.PreparedTurn) { p.Execution.Mode = "" },
		"symlink": func(t *testing.T, p *a.PreparedTurn) {
			if err := os.Symlink(t.TempDir(), filepath.Join(p.Sandbox.Verified.Workspace.Directory, "escape")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			turn := boundaryTurn(t, "container")
			mutate(t, &turn)
			engine := &adaptertest.Engine{}
			result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
			if err == nil || len(engine.Enforcers) != 0 || len(engine.Requests) != 0 {
				t.Fatalf("unsafe construction: %v", err)
			}
			if !result.IsError || result.Session != (a.BackendSession{}) {
				t.Fatalf("rejection is not a valid failure record: %+v", result)
			}
		})
	}
}
