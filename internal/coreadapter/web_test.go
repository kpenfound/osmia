package coreadapter_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	a "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
)

// webToolNames are the web tools of every backend.
var webToolNames = []string{"WebSearch", "WebFetch", "web_search", "websearch", "webfetch"}

func hasWebTool(tools []string) bool {
	return slices.ContainsFunc(tools, func(name string) bool { return slices.Contains(webToolNames, name) })
}

// webTurn is a read-only turn of backend in a container that holds Web.
func webTurn(t *testing.T, backend string) a.PreparedTurn {
	t.Helper()
	turn := toolTurn(t, "container")
	turn.Profile.Backend = backend
	turn.Sandbox.Verified.Capabilities.Web = true
	return turn
}

func runWebTurn(turn a.PreparedTurn, engine *adaptertest.Engine) (a.SessionResult, error) {
	return (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
}

// refuseWebGrants is an engine refusing any grants that name a web tool.
func refuseWebGrants(err error) func(agent.Grants) error {
	return func(g agent.Grants) error {
		if hasWebTool(g.Tools) {
			return err
		}
		return nil
	}
}

// dropWebTools is a policy that leaves out the web tools of its grants.
func dropWebTools(_ agent.Grants, p *agent.Policy) {
	p.Tools = slices.DeleteFunc(p.Tools, func(name string) bool { return slices.Contains(webToolNames, name) })
}

var webBackends = map[string][]string{
	"claude":   {"Read", "Glob", "Grep", "WebSearch", "WebFetch", "mcp__osmia_0"},
	"codex":    {"web_search", "mcp__osmia_0"},
	"opencode": {"read", "glob", "grep", "list", "websearch", "webfetch", "mcp__osmia_0"},
}

var backendWeb = map[string][]string{"claude": {"WebSearch", "WebFetch"}, "codex": {"web_search"}, "opencode": {"websearch", "webfetch"}}

var reducedBackends = map[string][]string{
	"claude":   {"Read", "Glob", "Grep", "mcp__osmia_0"},
	"codex":    {"mcp__osmia_0"},
	"opencode": {"read", "glob", "grep", "list", "mcp__osmia_0"},
}

func TestReadOnlyWebTurnHoldsOnlyWebTools(t *testing.T) {
	for backend, want := range webBackends {
		t.Run(backend, func(t *testing.T) {
			turn := webTurn(t, backend)
			engine := &adaptertest.Engine{}
			if _, err := runWebTurn(turn, engine); err != nil {
				t.Fatal(err)
			}
			if len(engine.Requests) != 1 || len(engine.Enforcers) != 1 || !engine.Released() {
				t.Fatalf("launches=%d enforcers=%d", len(engine.Requests), len(engine.Enforcers))
			}
			req := engine.Requests[0]
			if !reflect.DeepEqual(req.Grants.Tools, want) {
				t.Fatalf("tools %v, want %v", req.Grants.Tools, want)
			}
			for _, banned := range []string{"Write", "Edit", "Bash", "apply_patch", "shell", "edit", "bash"} {
				if slices.Contains(req.Grants.Tools, banned) {
					t.Fatalf("read-only web turn holds %s", banned)
				}
			}
			view := turn.Sandbox.Verified.Workspace.Directory
			if req.Grants.Mounts[0].Path != view || req.Grants.Mounts[0].Access != agent.ReadOnly || req.Grants.Mounts[1].Access != agent.ReadOnly {
				t.Fatalf("mounts %+v", req.Grants.Mounts)
			}
			if strings.Contains(req.SystemPrompt, "not available") {
				t.Fatalf("web turn told it has no web: %q", req.SystemPrompt)
			}
		})
	}
}

func TestCheckAdmitsWebOnlyForReadOnlyRoles(t *testing.T) {
	check := func(mutate func(*a.PreparedTurn)) error {
		turn := boundaryTurn(t, "container")
		mutate(&turn)
		return a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: &adaptertest.Engine{}}.Check(context.Background(), turn.Sandbox.Verified, turn.Execution)
	}
	if err := check(func(p *a.PreparedTurn) { p.Sandbox.Verified.Capabilities.Web = true }); err != nil {
		t.Fatalf("read-only web turn refused: %v", err)
	}
	for name, mutate := range map[string]func(*a.Capabilities){
		"write":   func(c *a.Capabilities) { c.WriteFiles = true },
		"execute": func(c *a.Capabilities) { c.Execute = true },
		"network": func(c *a.Capabilities) { c.Network = true },
	} {
		for _, web := range []bool{false, true} {
			err := check(func(p *a.PreparedTurn) {
				mutate(&p.Sandbox.Verified.Capabilities)
				p.Sandbox.Verified.Capabilities.Web = web
			})
			if !errors.Is(err, a.ErrUnsupported) {
				t.Fatalf("read-only %s (web=%v) admitted: %v", name, web, err)
			}
		}
	}
	for name, mutate := range map[string]func(*a.PreparedTurn){
		"domains": func(p *a.PreparedTurn) { p.Execution.Domains = []string{"example.com"} },
		"mounts":  func(p *a.PreparedTurn) { p.Execution.Mounts = []string{"/host"} },
	} {
		err := check(func(p *a.PreparedTurn) {
			p.Sandbox.Verified.Capabilities.Web = true
			mutate(p)
		})
		if !errors.Is(err, a.ErrUnsupported) {
			t.Fatalf("web turn with %s override admitted: %v", name, err)
		}
	}
}

func TestWebAdmitsNoFetchTool(t *testing.T) {
	fetch := a.Tool{Name: "fetch", Effect: a.ToolFetch}
	if a.ToolPermitted(a.Capabilities{Tools: []string{"fetch"}, Web: true}, fetch) {
		t.Fatal("Web admitted a fetch tool")
	}
	if !a.ToolPermitted(a.Capabilities{Tools: []string{"fetch"}, Network: true}, fetch) {
		t.Fatal("Network does not admit a fetch tool")
	}
	if a.ToolPermitted(a.Capabilities{Tools: []string{"fetch"}}, fetch) {
		t.Fatal("a fetch tool is admitted without a grant")
	}
}

func TestSessionPolicyHoldsWebToolsToTheGrant(t *testing.T) {
	for backend := range webBackends {
		t.Run(backend, func(t *testing.T) {
			// A session with the web tools of a web grant is accepted: the
			// unaltered fake session carries them.
			turn := webTurn(t, backend)
			engine := &adaptertest.Engine{}
			if _, err := runWebTurn(turn, engine); err != nil || len(engine.Requests) != 1 {
				t.Fatalf("err=%v launches=%d", err, len(engine.Requests))
			}
			// A session with web tools its grant lacks is refused without a
			// fallback.
			turn = toolTurn(t, "container")
			turn.Profile.Backend = backend
			engine = &adaptertest.Engine{Policy: func(p *agent.Policy) { p.Tools = append(p.Tools, backendWeb[backend]...) }}
			_, err := runWebTurn(turn, engine)
			if !errors.Is(err, a.ErrUnsupported) || len(engine.Requests) != 0 || len(engine.Enforcers) != 1 || !engine.Released() {
				t.Fatalf("err=%v launches=%d enforcers=%d", err, len(engine.Requests), len(engine.Enforcers))
			}
		})
	}
}

func TestEngineRefusalOfWebGrantRunsTurnWithoutWeb(t *testing.T) {
	for _, refused := range []error{agent.ErrUnsupported, agent.ErrNotGranted} {
		for backend, want := range reducedBackends {
			t.Run(fmt.Sprintf("%s/%v", backend, refused), func(t *testing.T) {
				turn := webTurn(t, backend)
				turn.SystemPrompt = "You plan."
				engine := &adaptertest.Engine{PrepareFor: refuseWebGrants(fmt.Errorf("fixture placement: %w", refused))}
				result, err := runWebTurn(turn, engine)
				if err != nil || result.FinalResponse != "fixture response" || result.IsError {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if len(engine.Enforcers) != 2 || len(engine.Requests) != 1 || !engine.Released() {
					t.Fatalf("enforcers=%d launches=%d released=%v", len(engine.Enforcers), len(engine.Requests), engine.Released())
				}
				req := engine.Requests[0]
				if !reflect.DeepEqual(req.Grants.Tools, want) {
					t.Fatalf("tools %v, want %v", req.Grants.Tools, want)
				}
				if !strings.HasPrefix(req.SystemPrompt, "You plan.") || !strings.Contains(req.SystemPrompt, "Web search and page fetching are not available on this turn") {
					t.Fatalf("degradation is not stated: %q", req.SystemPrompt)
				}
				if req.Grants.Mounts[0].Access != agent.ReadOnly {
					t.Fatalf("mounts %+v", req.Grants.Mounts)
				}
			})
		}
	}
}

func TestPolicyMismatchOfWebGrantRunsTurnWithoutWeb(t *testing.T) {
	for backend, want := range reducedBackends {
		t.Run(backend, func(t *testing.T) {
			turn := webTurn(t, backend)
			engine := &adaptertest.Engine{PolicyFor: dropWebTools}
			result, err := runWebTurn(turn, engine)
			if err != nil || result.FinalResponse != "fixture response" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if len(engine.Enforcers) != 2 || len(engine.Requests) != 1 || !engine.Released() {
				t.Fatalf("enforcers=%d launches=%d released=%v", len(engine.Enforcers), len(engine.Requests), engine.Released())
			}
			first, second := engine.Enforcers[0].Sessions(), engine.Enforcers[1].Sessions()
			if len(first) != 1 || len(second) != 1 {
				t.Fatalf("sessions %d and %d", len(first), len(second))
			}
			if !first[0].Released() || len(first[0].Requests()) != 0 {
				t.Fatalf("mismatched session released=%v ran=%d", first[0].Released(), len(first[0].Requests()))
			}
			if !second[0].Released() || len(second[0].Requests()) != 1 {
				t.Fatalf("reduced session released=%v ran=%d", second[0].Released(), len(second[0].Requests()))
			}
			req := engine.Requests[0]
			if !reflect.DeepEqual(req.Grants.Tools, want) || !strings.Contains(req.SystemPrompt, "not available on this turn") {
				t.Fatalf("tools %v prompt %q", req.Grants.Tools, req.SystemPrompt)
			}
		})
	}
}

func TestWebFallbackDoesNotWeakenRefusal(t *testing.T) {
	t.Run("reduced session that still mismatches is refused", func(t *testing.T) {
		turn := webTurn(t, "claude")
		engine := &adaptertest.Engine{PolicyFor: func(g agent.Grants, p *agent.Policy) {
			if hasWebTool(g.Tools) {
				dropWebTools(g, p)
				return
			}
			p.Tools = append(p.Tools, "Bash")
		}}
		result, err := runWebTurn(turn, engine)
		var refusal *a.UnsupportedError
		if !errors.As(err, &refusal) || refusal.Capability != "session policy" || !result.IsError {
			t.Fatalf("err=%v result=%+v", err, result)
		}
		if len(engine.Prepared) != 2 || len(engine.Requests) != 0 || !engine.Released() {
			t.Fatalf("prepared=%d launches=%d released=%v", len(engine.Prepared), len(engine.Requests), engine.Released())
		}
	})
	t.Run("engine refusing the reduced grants too is refused", func(t *testing.T) {
		turn := webTurn(t, "claude")
		engine := &adaptertest.Engine{PrepareErr: fmt.Errorf("fixture: %w", agent.ErrUnsupported)}
		_, err := runWebTurn(turn, engine)
		if !errors.Is(err, a.ErrUnsupported) || len(engine.Requests) != 0 || len(engine.Enforcers) != 2 {
			t.Fatalf("err=%v launches=%d enforcers=%d", err, len(engine.Requests), len(engine.Enforcers))
		}
	})
	t.Run("a policy mismatch unrelated to web tools is not retried", func(t *testing.T) {
		for name, tamper := range map[string]func(*agent.Policy){
			"shell tool": func(p *agent.Policy) { p.Tools = append(p.Tools, "Bash") },
			"write tool": func(p *agent.Policy) { p.Tools = append(p.Tools, "Write") },
			"VCS":        func(p *agent.Policy) { p.VCS = true },
			"all tools":  func(p *agent.Policy) { p.Tools = nil },
			"writable view": func(p *agent.Policy) {
				setAccess(p, p.Mounts[0].Path, agent.ReadWrite)
			},
		} {
			turn := webTurn(t, "claude")
			engine := &adaptertest.Engine{Policy: tamper}
			_, err := runWebTurn(turn, engine)
			var refusal *a.UnsupportedError
			if !errors.As(err, &refusal) || len(engine.Requests) != 0 || len(engine.Enforcers) != 1 || len(engine.Prepared) != 1 || !engine.Released() {
				t.Fatalf("%s: err=%v launches=%d enforcers=%d", name, err, len(engine.Requests), len(engine.Enforcers))
			}
		}
	})
	t.Run("a web mismatch that also has an unrelated one is not retried", func(t *testing.T) {
		turn := webTurn(t, "claude")
		engine := &adaptertest.Engine{PolicyFor: func(g agent.Grants, p *agent.Policy) {
			dropWebTools(g, p)
			p.Tools = append(p.Tools, "Bash")
		}}
		_, err := runWebTurn(turn, engine)
		if !errors.Is(err, a.ErrUnsupported) || len(engine.Requests) != 0 || len(engine.Enforcers) != 1 || !engine.Released() {
			t.Fatalf("err=%v launches=%d enforcers=%d", err, len(engine.Requests), len(engine.Enforcers))
		}
	})
	t.Run("a failure that is not a refusal is not retried", func(t *testing.T) {
		turn := webTurn(t, "claude")
		failure := errors.New("fixture I/O failure")
		engine := &adaptertest.Engine{PrepareFor: refuseWebGrants(failure)}
		_, err := runWebTurn(turn, engine)
		if !errors.Is(err, failure) || errors.Is(err, a.ErrUnsupported) || len(engine.Enforcers) != 1 || len(engine.Requests) != 0 {
			t.Fatalf("err=%v enforcers=%d", err, len(engine.Enforcers))
		}
	})
	t.Run("a turn without Web is not retried", func(t *testing.T) {
		turn := toolTurn(t, "container")
		engine := &adaptertest.Engine{PrepareErr: fmt.Errorf("fixture: %w", agent.ErrUnsupported)}
		_, err := runWebTurn(turn, engine)
		if !errors.Is(err, a.ErrUnsupported) || len(engine.Enforcers) != 1 {
			t.Fatalf("err=%v enforcers=%d", err, len(engine.Enforcers))
		}
	})
	t.Run("a turn that has run is not retried", func(t *testing.T) {
		for _, runErr := range []error{fmt.Errorf("fixture: %w", agent.ErrUnsupported), fmt.Errorf("fixture: %w", agent.ErrPolicyChanged), errors.New("fixture failure")} {
			turn := webTurn(t, "claude")
			engine := &adaptertest.Engine{RunErr: runErr}
			result, err := runWebTurn(turn, engine)
			if err == nil || !result.IsError || len(engine.Requests) != 1 || len(engine.Enforcers) != 1 || len(engine.Prepared) != 1 || !engine.Released() {
				t.Fatalf("err=%v launches=%d enforcers=%d prepared=%d", err, len(engine.Requests), len(engine.Enforcers), len(engine.Prepared))
			}
		}
	})
}
