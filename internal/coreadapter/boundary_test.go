package coreadapter_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
			// Only core can refuse these; every other widening is refused
			// before verification.
			verified := 2
			if coreRefuses[name] {
				verified = 3
			}
			if len(engine.Verified) != verified {
				t.Fatalf("verified %d requests, want %d", len(engine.Verified), verified)
			}
		})
	}
}

// coreRefuses names the widening requests that reach core's verification.
var coreRefuses = map[string]bool{"VCS": true, "profile env": true, "container env": true, "host MCP": true, "shell": true}

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
	if len(engine.Verified) != 1 || len(engine.Requests) != 1 {
		t.Fatal("verification or execution missing")
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

func TestHostSessionsAreRefused(t *testing.T) {
	for _, mode := range []string{"none", "claude"} {
		t.Run(mode, func(t *testing.T) {
			turn := boundaryTurn(t, mode)
			engine := &adaptertest.Engine{}
			_, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
			if !errors.Is(err, a.ErrUnsupported) || len(engine.Verified) != 0 || len(engine.Requests) != 0 {
				t.Fatalf("err=%v verified=%d launches=%d", err, len(engine.Verified), len(engine.Requests))
			}
		})
	}
}

func TestUnverifiedEngineNeverStarts(t *testing.T) {
	mutations := map[string]func(*agent.Turn){
		"no turn":                  nil,
		"VCS":                      func(p *agent.Turn) { p.VCS = true },
		"VCS executable":           func(p *agent.Turn) { p.DeniedExecutables = p.DeniedExecutables[1:] },
		"host environment":         func(p *agent.Turn) { p.Env = append(p.Env, "PATH=/usr/bin") },
		"delivery credentials":     func(p *agent.Turn) { p.Env = append(p.Env, "GH_TOKEN=secret") },
		"changed environment":      func(p *agent.Turn) { p.Env[0] = "LANG=en_US" },
		"built-in tools":           func(p *agent.Turn) { p.Tools = nil },
		"new tool":                 func(p *agent.Turn) { p.Tools = []string{"Bash"} },
		"write directory":          func(p *agent.Turn) { p.WriteDirs = []string{"/host"} },
		"writable read-only mount": func(p *agent.Turn) { p.Binds[0].Access = agent.ReadWrite },
		"alternate source":         func(p *agent.Turn) { p.Binds[0].Source = "/" },
		"alternate target":         func(p *agent.Turn) { p.Binds[0].Destination = "/" },
		"extra mount": func(p *agent.Turn) {
			p.Binds = append(p.Binds, agent.Bind{Source: "/host", Destination: "/host", Access: agent.ReadOnly})
		},
		"missing view": func(p *agent.Turn) { p.Binds = p.Binds[1:] },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			turn := boundaryTurn(t, "container")
			engine := &adaptertest.Engine{Mutate: mutate}
			executor := a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}
			if mutate == nil {
				executor.Runner = nilTurnEngine{engine}
			}
			_, err := (&a.TurnRunner{Executor: executor}).Run(context.Background(), turn)
			if !errors.Is(err, a.ErrUnsupported) || len(engine.Requests) != 0 {
				t.Fatalf("err=%v launches=%d", err, len(engine.Requests))
			}
		})
	}
}

// nilTurnEngine verifies nothing and reports no error.
type nilTurnEngine struct{ *adaptertest.Engine }

func (nilTurnEngine) Verify(agent.Request) (*agent.Turn, error) { return nil, nil }

func TestCoreVerificationFailureNeverStarts(t *testing.T) {
	turn := boundaryTurn(t, "container")
	for _, refusal := range []error{agent.ErrNotGranted, agent.ErrUnsupported, agent.ErrNoGrants} {
		engine := &adaptertest.Engine{VerifyErr: fmt.Errorf("fixture: %w", refusal)}
		_, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
		if !errors.Is(err, a.ErrUnsupported) || !errors.Is(err, refusal) || len(engine.Verified) != 1 || len(engine.Requests) != 0 {
			t.Fatalf("%v: err=%v launches=%d", refusal, err, len(engine.Requests))
		}
	}
	// Any other verification failure, such as an unreadable path, is not a
	// refusal and keeps its own error.
	failure := errors.New("fixture I/O failure")
	engine := &adaptertest.Engine{VerifyErr: failure}
	_, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
	if errors.Is(err, a.ErrUnsupported) || !errors.Is(err, failure) || len(engine.Requests) != 0 {
		t.Fatalf("err=%v launches=%d", err, len(engine.Requests))
	}
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
			if err == nil || len(engine.Verified) != 0 || len(engine.Requests) != 0 {
				t.Fatalf("unsafe construction: %v", err)
			}
			if !result.IsError || result.Session != (a.BackendSession{}) {
				t.Fatalf("rejection is not a valid failure record: %+v", result)
			}
		})
	}
}
