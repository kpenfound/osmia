package coreadapter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	a "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
)

func TestRawExecutionRequestCannotWidenBoundary(t *testing.T) {
	mutations := map[string]func(*agent.Request){
		"VCS":                func(r *agent.Request) { r.Profile.VCSAccess = true },
		"profile env":        func(r *agent.Request) { r.Profile.Env = map[string]string{"GH_TOKEN": "secret"} },
		"container env":      func(r *agent.Request) { r.ContainerEnv = map[string]string{"GH_TOKEN": "secret"} },
		"VCS env":            func(r *agent.Request) { r.VCSEnv = map[string]string{"GH_TOKEN": "secret"} },
		"VCS container env":  func(r *agent.Request) { r.VCSContainerEnv = map[string]string{"GH_TOKEN": "secret"} },
		"host MCP":           func(r *agent.Request) { r.HostMCP = &agent.HostMCP{} },
		"skills":             func(r *agent.Request) { r.Profile.Skills = []string{"repo-plugin"} },
		"shell":              func(r *agent.Request) { r.Profile.Shell = "/bin/sh" },
		"container override": func(r *agent.Request) { r.Profile.ContainerUseEnvironment = "other" },
		"tools":              func(r *agent.Request) { r.Profile.AllowedTools = []string{"Bash"} },
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
			engine := &adaptertest.IsolationEngine{}
			executor := a.BoundaryExecutor{Required: turn.Sandbox.Verified, Engine: engine}
			if _, err := (&a.TurnRunner{Executor: executor}).Run(context.Background(), turn); err != nil {
				t.Fatal(err)
			}
			req := engine.Requests[0]
			mutate(&req)
			if _, err := executor.Run(context.Background(), req, turn.Execution); !errors.Is(err, a.ErrUnsupported) {
				t.Fatal(err)
			}
			if len(engine.Policies) != 1 || len(engine.Requests) != 1 {
				t.Fatal("raw override reached construction")
			}
		})
	}
}

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

func TestHostAndContainerConstruction(t *testing.T) {
	for _, mode := range []string{"none", "container"} {
		t.Run(mode, func(t *testing.T) {
			for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "SSH_AUTH_SOCK", "OPENAI_API_KEY", "HOME", "PATH"} {
				t.Setenv(key, "host-secret")
			}
			turn := boundaryTurn(t, mode)
			engine := &adaptertest.IsolationEngine{}
			runner := a.TurnRunner{Executor: a.BoundaryExecutor{Required: turn.Sandbox.Verified, Engine: engine}}
			if _, err := runner.Run(context.Background(), turn); err != nil {
				t.Fatal(err)
			}
			if len(engine.Requests) != 1 || engine.Released != 1 {
				t.Fatal("execution or cleanup missing")
			}
			policy, req := engine.Policies[0], engine.Requests[0]
			if policy.Mode != mode || len(policy.Mounts) != 1 || !policy.Mounts[0].ReadOnly || policy.Mounts[0].Source != turn.Sandbox.Verified.Workspace.Directory || !policy.NoVCS || !policy.NoConfigDiscovery || !policy.NoExtraTools || !policy.NoHostFiles || !policy.NoHostEnvironment || !policy.NoDeliveryCredentials || !policy.NoPrivilegeEscalation {
				t.Fatalf("policy: %+v", policy)
			}
			if !reflect.DeepEqual(req.Env, map[string]string{"LANG": "C"}) || req.Profile.VCSAccess || req.Workspace.VCS() != nil || len(req.Profile.MCP) != 0 || len(req.Profile.AllowedTools) != 0 {
				t.Fatalf("ambient access: %+v", req)
			}
		})
	}
}

func TestUnverifiedEngineNeverStarts(t *testing.T) {
	mutations := map[string]func(*a.BoundaryPolicy){
		"VCS executable":           func(p *a.BoundaryPolicy) { p.NoVCS = false },
		"host environment":         func(p *a.BoundaryPolicy) { p.NoHostEnvironment = false },
		"delivery credentials":     func(p *a.BoundaryPolicy) { p.NoDeliveryCredentials = false },
		"host files":               func(p *a.BoundaryPolicy) { p.NoHostFiles = false },
		"tool discovery":           func(p *a.BoundaryPolicy) { p.NoExtraTools = false },
		"repository configuration": func(p *a.BoundaryPolicy) { p.NoConfigDiscovery = false },
		"privilege escalation":     func(p *a.BoundaryPolicy) { p.NoPrivilegeEscalation = false },
		"writable read-only mount": func(p *a.BoundaryPolicy) { p.Mounts[0].ReadOnly = false },
		"alternate source":         func(p *a.BoundaryPolicy) { p.Mounts[0].Source = "/" },
		"alternate target":         func(p *a.BoundaryPolicy) { p.Mounts[0].Target = "/" },
		"extra mount":              func(p *a.BoundaryPolicy) { p.Mounts = append(p.Mounts, a.Mount{Source: "/host", Target: "/host"}) },
		"write override":           func(p *a.BoundaryPolicy) { p.Isolation.Capabilities.WriteFiles = true },
		"execute override":         func(p *a.BoundaryPolicy) { p.Isolation.Capabilities.Execute = true },
		"network override":         func(p *a.BoundaryPolicy) { p.Isolation.Capabilities.Network = true },
		"new tool":                 func(p *a.BoundaryPolicy) { p.Isolation.Capabilities.Tools = []string{"Bash"} },
		"secret":                   func(p *a.BoundaryPolicy) { p.Isolation.Environment["GH_TOKEN"] = "secret" },
	}
	for _, mode := range []string{"none", "container"} {
		for name, mutate := range mutations {
			t.Run(mode+"/"+name, func(t *testing.T) {
				turn := boundaryTurn(t, mode)
				engine := &adaptertest.IsolationEngine{Mutate: mutate}
				_, err := (&a.TurnRunner{Executor: a.BoundaryExecutor{Required: turn.Sandbox.Verified, Engine: engine}}).Run(context.Background(), turn)
				if !errors.Is(err, a.ErrUnsupported) || len(engine.Requests) != 0 || engine.Released != 1 {
					t.Fatalf("err=%v launches=%d releases=%d", err, len(engine.Requests), engine.Released)
				}
			})
		}
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
			engine := &adaptertest.IsolationEngine{}
			_, err := (&a.TurnRunner{Executor: a.BoundaryExecutor{Required: turn.Sandbox.Verified, Engine: engine}}).Run(context.Background(), turn)
			if err == nil || len(engine.Policies) != 0 || len(engine.Requests) != 0 {
				t.Fatalf("unsafe construction: %v", err)
			}
		})
	}
}
