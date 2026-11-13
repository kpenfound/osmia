package coreadapter_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/agentcore/mcphost"
	"github.com/kpenfound/agentcore/vcs"
	a "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sbxRunner uses a fake sbx CLI and the supplied agent binary in place of
// the costly, externally mutating sandbox daemon and model session. It leaves
// unverified whether a real sbx sandbox and backend behave this way; these
// tests only check what core's enforcer and CoreExecutor do with the CLI's
// output and policy.
func sbxRunner(t *testing.T, bin string) agent.Runner {
	t.Helper()
	sbx := agenttest.Sbx(t, a.TokenEnvironment)
	// The daemon refuses empty command elements before starting the agent.
	data := readSbxFile(t, sbx)
	data = strings.Replace(data, "set -e\n", `set -e
if [ "$1" = exec ]; then
  for arg in "$@"; do
    if [ -z "$arg" ]; then echo 'cmd element is empty' >&2; exit 1; fi
  done
fi
`, 1)
	if err := os.WriteFile(sbx, []byte(data), 0700); err != nil {
		t.Fatal(err)
	}
	return agent.Runner{SbxBin: sbx, ClaudeBin: bin, CodexBin: bin, OpenCodeBin: bin}
}

func readSbxFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSbxRunsScopedTurnsThroughCoreWithFakeSandbox(t *testing.T) {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(name, "host-only-secret")
	}
	for _, backend := range []string{"claude", "codex", "opencode"} {
		for _, image := range []string{"", "example/template:1"} {
			t.Run(backend+"/"+image, func(t *testing.T) {
				turn := toolTurn(t, "sbx")
				turn.Execution.Image = image
				turn.Profile.Backend = backend
				turn.Profile.Timeout = time.Minute
				if image != "" {
					turn.Scope.Role = "mason"
					turn.Sandbox.Verified.Workspace.Access = a.ReadWrite
					turn.Sandbox.Verified.Capabilities.WriteFiles = true
					turn.Sandbox.Verified.Capabilities.Execute = true
				}
				turn.Sandbox.Verified.Environment[a.TokenEnvironment] = turn.SessionDirectory
				var bin string
				switch backend {
				case agent.AgentClaude, agent.AgentCodex:
					bin = agenttest.Session(t, backend, agenttest.Turn{ResultText: "boxed", SessionID: "fixture"}).Bin
				case "opencode":
					bin = agenttest.Script(t, "fake-agent", `case "$*" in
*--version*) echo '2.1.200'; exit 0 ;;
*--help*) echo '--tools --strict-mcp-config'; exit 0 ;;
esac
if [ "${BUN_BE_BUN:-}" = 1 ]; then
  echo '`+agenttest.OpenCodeNoCustomTools+`'
  exit 0
fi
if [ "$1" = --pure ] && [ "$2" = debug ]; then printf '%s\n' "$OPENCODE_CONFIG_CONTENT"; exit 0; fi
cat >/dev/null
echo '{"type":"text","sessionID":"fixture","part":{"type":"text","text":"boxed"}}'
echo '{"type":"step_finish","sessionID":"fixture","part":{"type":"step-finish","reason":"stop","cost":0}}'
`)
				}
				runner := sbxRunner(t, bin)
				result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: a.CoreEngine{Runner: runner}}}).Run(context.Background(), turn)
				if err != nil || result.IsError || result.FinalResponse != "boxed" {
					t.Fatalf("result %+v: %v", result, err)
				}
				created := readSbxFile(t, filepath.Join(filepath.Dir(runner.SbxBin), "sbx-create.txt"))
				view := turn.Sandbox.Verified.Workspace.Directory
				mounts := []string{view + "\n"}
				if image == "" {
					mounts = []string{view + ":ro\n", filepath.Join(turn.SessionDirectory, "work") + "\n"}
				}
				for _, want := range append(mounts, "--skills\noff\n", backend+"\n", turn.SessionDirectory+":ro\n") {
					if !strings.Contains(created, want) {
						t.Fatalf("missing %q in sandbox mounts: %s", want, created)
					}
				}
				if strings.Contains(created, "--template\n") != (image != "") || image != "" && !strings.Contains(created, "--template\n"+image+"\n") {
					t.Fatalf("template: %s", created)
				}
				args := readSbxFile(t, filepath.Join(turn.SessionDirectory, "sbx-exec-args.txt"))
				if image == "" {
					// The writable scratch directory has a read-only parent;
					// sbx needs a separate primary workspace for startup writes.
					_, workspaces, ok := strings.Cut(created, backend+"\n")
					if !ok {
						t.Fatalf("missing backend in create arguments: %s", created)
					}
					primary := strings.Split(workspaces, "\n")[0]
					if primary == "" || strings.HasSuffix(primary, ":ro") || strings.HasPrefix(primary, turn.SessionDirectory+string(filepath.Separator)) || primary == view {
						t.Fatalf("startup workspace is not separate from protected inputs: %s", created)
					}
					if _, err := os.Stat(primary); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("startup workspace remains after cleanup: %s: %v", primary, err)
					}
					if !strings.Contains(args, "--workdir\n"+filepath.Join(turn.SessionDirectory, "work")+"\n") {
						t.Fatalf("agent did not run in its granted scratch directory: %s", args)
					}
				}
				for _, forbidden := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "_EXPERIMENTAL_DAGGER_RUNNER_HOST"} {
					if strings.Contains(args, forbidden) {
						t.Fatalf("sandbox receives %s", forbidden)
					}
				}
				assertSbxHostPolicy(t, runner, "1")
				if !strings.Contains(readSbxFile(t, filepath.Join(filepath.Dir(runner.SbxBin), "sbx-rm.txt")), "rm\n--force\n") {
					t.Fatal("sandbox not removed")
				}
			})
		}
	}
}

func TestSbxRefusesUnavailableCLI(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint("missing=", missing), func(t *testing.T) {
			runner := sbxRunner(t, agenttest.Script(t, "fake-agent", "exit 99\n"))
			if missing {
				runner.SbxBin = "/nonexistent/sbx"
			} else if err := os.WriteFile(filepath.Join(filepath.Dir(runner.SbxBin), "fail-version"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			turn := toolTurn(t, "sbx")
			_, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: a.CoreEngine{Runner: runner}}}).Run(context.Background(), turn)
			if !errors.Is(err, a.ErrUnsupported) {
				t.Fatalf("unavailable CLI: %v", err)
			}
		})
	}
}

// assertSbxHostPolicy checks that the sandbox was allowed to reach the host's
// MCP port, and that the rule and the sandbox were removed afterwards.
func assertSbxHostPolicy(t *testing.T, runner agent.Runner, port string) {
	t.Helper()
	dir := filepath.Dir(runner.SbxBin)
	created := strings.Split(readSbxFile(t, filepath.Join(dir, "sbx-create.txt")), "\n")
	name := ""
	for i, arg := range created {
		if arg == "--name" && i+1 < len(created) {
			name = created[i+1]
		}
	}
	if name == "" {
		t.Fatal("sandbox not named")
	}
	want := "policy allow network --sandbox " + name + " localhost:" + port + "\n" +
		"policy rm network --sandbox " + name + " --resource localhost:" + port + " --force\n"
	calls := "create --no-progress\npolicy allow\npolicy rm\nrm --force\n"
	if got := readSbxFile(t, filepath.Join(dir, "sbx-policy.txt")); got != want {
		t.Fatalf("host policy:\n%s\nwant:\n%s", got, want)
	}
	if got := readSbxFile(t, filepath.Join(dir, "sbx-calls.txt")); got != calls {
		t.Fatalf("sandbox lifecycle:\n%s\nwant:\n%s", got, calls)
	}
}

func TestSbxHostGrantsAndAgentPolicyCannotWiden(t *testing.T) {
	for name, mutate := range map[string]func(*agent.Policy){
		"agent":              func(p *agent.Policy) { p.Agent = "opencode" },
		"missing host grant": func(p *agent.Policy) { p.HostServers = nil },
		"other port":         func(p *agent.Policy) { p.HostServers[0].Port++ },
		"extra host grant":   func(p *agent.Policy) { p.HostServers = append(p.HostServers, agent.HostServer{Name: "other", Port: 2}) },
		"Dagger":             func(p *agent.Policy) { p.DaggerEngine = "tcp://localhost:1234" },
	} {
		t.Run(name, func(t *testing.T) {
			turn := toolTurn(t, "sbx")
			engine := &adaptertest.Engine{Policy: mutate}
			_, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
			if !errors.Is(err, a.ErrUnsupported) || len(engine.Requests) != 0 || !engine.Released() {
				t.Fatalf("widened policy: %v, launches=%d", err, len(engine.Requests))
			}
		})
	}
}

func TestSbxHostEndpointsRequireExplicitLocalPorts(t *testing.T) {
	for _, endpoint := range []string{"http://remote.example:1234/mcp", "http://localhost/mcp", "http://localhost:0/mcp", "http://localhost:65536/mcp", "http://localhost:*/mcp", "http://host.docker.internal/mcp"} {
		t.Run(endpoint, func(t *testing.T) {
			turn := toolTurn(t, "sbx")
			turn.MCP[0].URL = endpoint
			engine := &adaptertest.Engine{}
			_, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
			if !errors.Is(err, a.ErrUnsupported) || len(engine.Prepared) != 0 {
				t.Fatalf("nonlocal/unbounded endpoint: %v", err)
			}
		})
	}
}

func TestSbxSharedMCPPortKeepsCallerOwnedListener(t *testing.T) {
	for _, failCleanup := range []bool{false, true} {
		t.Run(fmt.Sprint("cleanup failure=", failCleanup), func(t *testing.T) {
			ctx := context.Background()
			endpoint, lease, err := a.SbxTransport().Start(ctx, mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release(ctx)
			turn := toolTurn(t, "sbx")
			turn.MCP = []a.Endpoint{endpoint, endpoint}
			turn.Sandbox.Verified.Environment[a.TokenEnvironment] = endpoint.Token
			turn.Sandbox.Verified.Environment["LANG"] = turn.SessionDirectory
			runner := sbxRunner(t, agenttest.Session(t, agent.AgentClaude, agenttest.Turn{ResultText: "boxed"}).Bin)
			runner.SbxBin = agenttest.Sbx(t, "LANG")
			if failCleanup {
				if err := os.WriteFile(filepath.Join(filepath.Dir(runner.SbxBin), "fail-policy-rm"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: a.CoreEngine{Runner: runner}}}).Run(ctx, turn)
			if err != nil || result.IsError {
				t.Fatalf("turn: %+v, %v", result, err)
			}
			address, err := url.Parse(endpoint.URL)
			if err != nil {
				t.Fatal(err)
			}
			assertSbxHostPolicy(t, runner, address.Port())
			address.Host = "127.0.0.1:" + address.Port()
			client := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: address.String(), HTTPClient: &http.Client{Transport: mcphost.BearerTransport(endpoint.Token, nil)}, MaxRetries: -1}, nil)
			if err != nil {
				t.Fatalf("sandbox cleanup stopped caller's MCP listener: %v", err)
			}
			defer session.Close()
			if _, err := session.ListTools(ctx, nil); err != nil {
				t.Fatalf("authenticated listener after sandbox removal: %v", err)
			}
		})
	}
}

func TestSbxRequestCannotChangePreparedHostPort(t *testing.T) {
	turn := toolTurn(t, "sbx")
	engine := &adaptertest.Engine{}
	executor := a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}
	if _, err := (&a.TurnRunner{Executor: executor}).Run(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	req := engine.Requests[0]
	req.Workspace = vcs.Directory(turn.Sandbox.Verified.Workspace.Directory)
	entry := req.Profile.MCP["osmia_0"]
	entry.URL = "http://127.0.0.1:2/mcp"
	req.Profile.MCP["osmia_0"] = entry
	if _, err := executor.Run(context.Background(), req, turn.Execution); !errors.Is(err, a.ErrUnsupported) {
		t.Fatalf("changed host endpoint: %v", err)
	}
	if len(engine.Prepared) != 1 || len(engine.Requests) != 1 {
		t.Fatal("changed endpoint reached preparation")
	}
}

// A turn's sandbox size reaches the profile of its request, and a turn that
// sets none leaves the profile's size unset.
func TestSandboxSizeReachesTheProfile(t *testing.T) {
	for name, c := range map[string]struct {
		cpus   int
		memory string
	}{
		"unset":  {},
		"cpus":   {cpus: 2},
		"memory": {memory: "512m"},
		"both":   {cpus: 4, memory: "4g"},
	} {
		t.Run(name, func(t *testing.T) {
			turn := toolTurn(t, "sbx")
			turn.Execution.CPUs, turn.Execution.Memory = c.cpus, c.memory
			engine := &adaptertest.Engine{}
			if _, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn); err != nil {
				t.Fatal(err)
			}
			if len(engine.Requests) != 1 || engine.Requests[0].Profile.SandboxCPUs != c.cpus || engine.Requests[0].Profile.SandboxMemory != c.memory {
				t.Fatalf("requests %+v", engine.Requests)
			}
		})
	}
}

// A size that is not one a sandbox can be created with, or a size for a
// sandbox that is not sbx, never reaches a session.
func TestSandboxSizeIsRefusedWhereItCannotApply(t *testing.T) {
	for name, mutate := range map[string]func(*a.PreparedTurn){
		"negative cpus":       func(turn *a.PreparedTurn) { turn.Execution.CPUs = -1 },
		"memory with options": func(turn *a.PreparedTurn) { turn.Execution.Memory = "4g --privileged" },
		"memory without unit": func(turn *a.PreparedTurn) { turn.Execution.Memory = "4096" },
		"cpus in a container": func(turn *a.PreparedTurn) {
			turn.Execution.Mode, turn.Execution.Image, turn.Execution.CPUs = "container", "fixture-image", 2
		},
		"memory on the host": func(turn *a.PreparedTurn) { turn.Execution.Mode, turn.Execution.Memory = "none", "4g" },
	} {
		t.Run(name, func(t *testing.T) {
			turn := toolTurn(t, "sbx")
			mutate(&turn)
			engine := &adaptertest.Engine{}
			_, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
			if !errors.Is(err, a.ErrUnsupported) || len(engine.Requests) != 0 || len(engine.Prepared) != 0 {
				t.Fatalf("err=%v prepared=%d launches=%d", err, len(engine.Prepared), len(engine.Requests))
			}
		})
	}
}

// A request whose size differs from the role's settings widens the service
// boundary and is refused.
func TestSandboxSizeCannotBeChangedAfterPreparation(t *testing.T) {
	turn := toolTurn(t, "sbx")
	turn.Execution.CPUs, turn.Execution.Memory = 2, "4g"
	engine := &adaptertest.Engine{}
	executor := a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}
	if _, err := (&a.TurnRunner{Executor: executor}).Run(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*agent.Profile){
		"cpus":   func(p *agent.Profile) { p.SandboxCPUs = 64 },
		"memory": func(p *agent.Profile) { p.SandboxMemory = "32g" },
		"unset":  func(p *agent.Profile) { p.SandboxCPUs, p.SandboxMemory = 0, "" },
	} {
		t.Run(name, func(t *testing.T) {
			req := engine.Requests[0]
			req.Workspace = vcs.Directory(turn.Sandbox.Verified.Workspace.Directory)
			mutate(&req.Profile)
			if _, err := executor.Run(context.Background(), req, turn.Execution); !errors.Is(err, a.ErrUnsupported) {
				t.Fatalf("changed size: %v", err)
			}
		})
	}
}

// A turn's size becomes sbx create's --cpus and --memory among its options,
// before the agent, and a turn without one has the command line it always had.
func TestSbxCreateReceivesTheSandboxSize(t *testing.T) {
	for name, c := range map[string]struct {
		cpus   int
		memory string
		extra  []string
	}{
		"unset":  {},
		"cpus":   {cpus: 2, extra: []string{"--cpus", "2"}},
		"memory": {memory: "512m", extra: []string{"--memory", "512m"}},
		"both":   {cpus: 4, memory: "4g", extra: []string{"--cpus", "4", "--memory", "4g"}},
	} {
		for _, image := range []string{"", "example/template:1"} {
			t.Run(name+"/"+image, func(t *testing.T) {
				turn := toolTurn(t, "sbx")
				turn.Execution.Image, turn.Execution.CPUs, turn.Execution.Memory = image, c.cpus, c.memory
				turn.Profile.Timeout = time.Minute
				turn.Sandbox.Verified.Environment[a.TokenEnvironment] = turn.SessionDirectory
				runner := sbxRunner(t, agenttest.Session(t, agent.AgentClaude, agenttest.Turn{ResultText: "boxed"}).Bin)
				result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: a.CoreEngine{Runner: runner}}}).Run(context.Background(), turn)
				if err != nil || result.IsError {
					t.Fatalf("result %+v: %v", result, err)
				}
				args := strings.Split(strings.TrimSuffix(readSbxFile(t, filepath.Join(filepath.Dir(runner.SbxBin), "sbx-create.txt")), "\n"), "\n")
				want := []string{"create", "--no-progress", "--name", args[3], "--skills", "off"}
				if image != "" {
					want = append(want, "--template", image)
				}
				want = append(append(want, c.extra...), agent.AgentClaude)
				if len(args) <= len(want) || !slices.Equal(args[:len(want)], want) {
					t.Fatalf("sbx create arguments:\n%q\nwant the prefix\n%q", args, want)
				}
				if c.cpus == 0 && slices.Contains(args, "--cpus") || c.memory == "" && slices.Contains(args, "--memory") {
					t.Fatalf("a size nothing set: %q", args)
				}
			})
		}
	}
}
