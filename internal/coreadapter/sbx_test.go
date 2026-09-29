package coreadapter_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/mcphost"
	"github.com/kpenfound/busybees/core/vcs"
	a "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func sbxRunner(t *testing.T, body string) agent.Runner {
	t.Helper()
	bin := agenttest.Script(t, "fake-agent", body)
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
				body := `case "$*" in
*--version*) echo '2.1.200'; exit 0 ;;
*--help*) echo '--tools --strict-mcp-config'; exit 0 ;;
esac
if [ "$1" = features ]; then
case "$*" in
*features.shell_tool=false*) echo 'shell_tool stable false' ;;
*) echo 'shell_tool stable true' ;;
esac
exit 0
fi
if [ "$1" = mcp ]; then
  echo '[{"name":"osmia_0","enabled":true,"transport":{"type":"streamable_http","url":"http://host.docker.internal:1/mcp","bearer_token_env_var":"OSMIA_MCP_TOKEN"}}]'
  exit 0
fi
if [ "$1" = app-server ]; then
  read -r init
  read -r initialized
  read -r config
  echo '{"id":2,"result":{"config":{},"origins":{"approval_policy":{"name":{"type":"sessionFlags"}},"orchestrator.mcp.enabled":{"name":{"type":"sessionFlags"}},"agents.enabled":{"name":{"type":"sessionFlags"}},"tools.experimental_request_user_input.enabled":{"name":{"type":"sessionFlags"}},"tools.update_plan.enabled":{"name":{"type":"sessionFlags"}},"web_search":{"name":{"type":"sessionFlags"}}}}}'
  exit 0
fi
if [ "${BUN_BE_BUN:-}" = 1 ]; then
  echo '` + agenttest.OpenCodeNoCustomTools + `'
  exit 0
fi
if [ "$1" = --pure ] && [ "$2" = debug ]; then printf '%s\n' "$OPENCODE_CONFIG_CONTENT"; exit 0; fi
cat >/dev/null
`
				switch backend {
				case "claude":
					body += "echo '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"boxed\"}'\n"
				case "codex":
					body += "echo '{\"type\":\"thread.started\",\"thread_id\":\"fixture\"}'\necho '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"boxed\"}}'\necho '{\"type\":\"turn.completed\"}'\n"
				case "opencode":
					body += "echo '{\"type\":\"text\",\"sessionID\":\"fixture\",\"part\":{\"type\":\"text\",\"text\":\"boxed\"}}'\necho '{\"type\":\"step_finish\",\"sessionID\":\"fixture\",\"part\":{\"type\":\"step-finish\",\"reason\":\"stop\",\"cost\":0}}'\n"
				}
				runner := sbxRunner(t, body)
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
				assertSbxHostPolicy(t, runner, "1", true)
				if !strings.Contains(readSbxFile(t, filepath.Join(filepath.Dir(runner.SbxBin), "sbx-rm.txt")), "rm\n--force\n") {
					t.Fatal("sandbox not removed")
				}
			})
		}
	}
}

func TestSbxPreparedSessionRefusesChangedGrantsAndRelease(t *testing.T) {
	ctx := context.Background()
	runner := sbxRunner(t, "exit 99\n")
	work, sessionDir := t.TempDir(), t.TempDir()
	grants := agent.Grants{Tools: []string{}, Mounts: []agent.Mount{{Path: work, Access: agent.ReadWrite}, {Path: sessionDir, Access: agent.ReadOnly}}}
	enforcer, err := a.NewEnforcer(runner, a.ExecutionSettings{Mode: "sbx"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := enforcer.Prepare(ctx, grants)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Release(ctx)
	p := session.Policy()
	p.Mounts[0].Access = agent.ReadOnly
	if !session.Policy().Writes(work) {
		t.Fatal("returned policy aliases session state")
	}
	req := agent.Request{Profile: agent.Profile{Agent: "claude", Sandbox: "sbx"}, Workspace: vcs.Directory(work), SessionDir: sessionDir}
	grants.VCS = true
	req.Grants = &grants
	if _, err := session.Run(ctx, req); !errors.Is(err, agent.ErrNotGranted) {
		t.Fatalf("changed grants: %v", err)
	}
	req.Grants = nil
	if err := os.Remove(work); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), work); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(ctx, req); !errors.Is(err, agent.ErrPolicyChanged) {
		t.Fatalf("changed mount: %v", err)
	}
	if err := session.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(ctx, req); !errors.Is(err, agent.ErrReleased) {
		t.Fatalf("released session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(runner.SbxBin), "sbx-create.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused request launched a sandbox")
	}
}

func TestSbxRefusesUnavailableCLI(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint("missing=", missing), func(t *testing.T) {
			runner := sbxRunner(t, "exit 99\n")
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

func TestSbxFailureAndCancellationRemoveSandbox(t *testing.T) {
	for _, cancelTurn := range []bool{false, true} {
		name := "failure"
		if cancelTurn {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			turn := toolTurn(t, "sbx")
			turn.Sandbox.Verified.Environment[a.TokenEnvironment] = turn.SessionDirectory
			body := "exit 42\n"
			if cancelTurn {
				body = "touch \"$OSMIA_MCP_TOKEN/started\"\nexec sleep 60\n"
			}
			runner := sbxRunner(t, body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: a.CoreEngine{Runner: runner}}}).Run(ctx, turn)
				if err == nil && !result.IsError {
					t.Error("failed session reported success")
				}
				done <- err
			}()
			if cancelTurn {
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(turn.SessionDirectory, "started")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						cancel()
						<-done
						t.Fatal("fake agent did not start")
					}
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
			}
			select {
			case err := <-done:
				if cancelTurn && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("sandbox run did not finish")
			}
			assertSbxHostPolicy(t, runner, "1", true)
			removed := readSbxFile(t, filepath.Join(filepath.Dir(runner.SbxBin), "sbx-rm.txt"))
			if strings.Count(removed, "rm\n--force\n") != 1 {
				t.Fatalf("cleanup: %s", removed)
			}
		})
	}
}

func assertSbxHostPolicy(t *testing.T, runner agent.Runner, port string, removed bool) {
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
	want := "policy allow network --sandbox " + name + " localhost:" + port + "\n"
	calls := "create --quiet\npolicy allow\n"
	if removed {
		want += "policy rm network --sandbox " + name + " --resource localhost:" + port + " --force\n"
		calls += "policy rm\n"
	}
	calls += "rm --force\n"
	if got := readSbxFile(t, filepath.Join(dir, "sbx-policy.txt")); got != want {
		t.Fatalf("host policy:\n%s\nwant:\n%s", got, want)
	}
	if got := readSbxFile(t, filepath.Join(dir, "sbx-calls.txt")); got != calls {
		t.Fatalf("sandbox lifecycle:\n%s\nwant:\n%s", got, calls)
	}
}

func TestSbxPolicyFailurePreventsExecutionAndRemovesSandbox(t *testing.T) {
	turn := toolTurn(t, "sbx")
	runner := sbxRunner(t, "exit 99\n")
	dir := filepath.Dir(runner.SbxBin)
	if err := os.WriteFile(filepath.Join(dir, "fail-policy"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: a.CoreEngine{Runner: runner}}}).Run(context.Background(), turn)
	if err == nil || !strings.Contains(err.Error(), "allow sandbox") || !result.IsError {
		t.Fatalf("policy refusal: %+v, %v", result, err)
	}
	for _, name := range []string{"sbx-probe.txt", "sbx-setup.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("policy refusal reached backend: %s", name)
		}
	}
	assertSbxHostPolicy(t, runner, "1", false)
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
			runner := sbxRunner(t, "cat >/dev/null\necho '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"boxed\"}'\n")
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
			assertSbxHostPolicy(t, runner, address.Port(), true)
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
