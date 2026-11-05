package coreadapter_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	a "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
)

// daggerTurn is a mason turn in sbx that may execute commands and asks for
// the Dagger CLI against an engine on the host's loopback.
func daggerTurn(t *testing.T) a.PreparedTurn {
	t.Helper()
	turn := toolTurn(t, "sbx")
	turn.Scope.Role = "mason"
	turn.Profile.Timeout = time.Minute
	turn.Sandbox.Verified.Workspace.Access = a.ReadWrite
	turn.Sandbox.Verified.Capabilities.WriteFiles = true
	turn.Sandbox.Verified.Capabilities.Execute = true
	turn.Execution.Dagger = &agent.Dagger{Engine: "tcp://127.0.0.1:1234", Version: "v0.20.5"}
	return turn
}

func TestSbxMasonTurnGetsDaggerCLIAndEngine(t *testing.T) {
	for name, c := range map[string]struct {
		engine, template string
		install          bool
	}{
		"install":          {engine: "tcp://127.0.0.1:1234", install: true},
		"template release": {engine: "tcp://127.0.0.1:1234", template: "v0.20.5"},
		"template other":   {engine: "tcp://127.0.0.1:1234", template: "v0.19.0", install: true},
		"container engine": {engine: "docker-container://dagger-engine-v0.20.5", install: true},
	} {
		t.Run(name, func(t *testing.T) {
			turn := daggerTurn(t)
			turn.Execution.Dagger.Engine = c.engine
			turn.Sandbox.Verified.Environment[a.TokenEnvironment] = turn.SessionDirectory
			runner := sbxRunner(t, agenttest.Session(t, agent.AgentClaude, agenttest.Turn{ResultText: "built"}).Bin)
			dir := filepath.Dir(runner.SbxBin)
			runner.DockerBin = agenttest.Script(t, "docker", "exec cat\n")
			if c.template != "" {
				if err := os.WriteFile(filepath.Join(dir, "dagger-version"), []byte(c.template), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: a.CoreEngine{Runner: runner}}}).Run(context.Background(), turn)
			if err != nil || result.IsError || result.FinalResponse != "built" {
				t.Fatalf("result %+v: %v", result, err)
			}
			setup, err := os.ReadFile(filepath.Join(dir, "sbx-setup.txt"))
			installed := err == nil && strings.Contains(string(setup), "DAGGER_VERSION=0.20.5\n") && strings.Contains(string(setup), agent.DaggerInstallScript)
			if installed != c.install {
				t.Fatalf("installed %v, want %v:\n%s", installed, c.install, setup)
			}
			env := readSbxFile(t, filepath.Join(turn.SessionDirectory, "sbx-exec-env.txt"))
			prefix := agent.EnvDaggerRunnerHost + "=tcp://host.docker.internal:"
			port := ""
			for _, line := range strings.Split(env, "\n") {
				if p, ok := strings.CutPrefix(line, prefix); ok {
					port = p
				}
			}
			if port == "" || c.engine == "tcp://127.0.0.1:1234" && port != "1234" {
				t.Fatalf("session does not reach the host engine:\n%s", env)
			}
			policy := readSbxFile(t, filepath.Join(dir, "sbx-policy.txt"))
			for _, want := range []string{" localhost:" + port + "\n", " --resource localhost:" + port + " --force\n"} {
				if !strings.Contains(policy, want) {
					t.Fatalf("engine port rule %q missing:\n%s", want, policy)
				}
			}
		})
	}
}

func TestDaggerRequiresAnSbxTurnThatExecutes(t *testing.T) {
	ctx := context.Background()
	for name, mutate := range map[string]func(*a.PreparedTurn){
		"container": func(turn *a.PreparedTurn) {
			turn.Execution.Mode, turn.Execution.Image = "container", "fixture-image"
		},
		"host":          func(turn *a.PreparedTurn) { turn.Execution.Mode = "none" },
		"no execute":    func(turn *a.PreparedTurn) { turn.Sandbox.Verified.Capabilities.Execute = false },
		"version":       func(turn *a.PreparedTurn) { turn.Execution.Dagger.Version = "latest" },
		"engine":        func(turn *a.PreparedTurn) { turn.Execution.Dagger.Engine = "image://registry.dagger.io/engine:v0.20.5" },
		"relative sock": func(turn *a.PreparedTurn) { turn.Execution.Dagger.Engine = "unix://engine.sock" },
	} {
		t.Run(name, func(t *testing.T) {
			turn := daggerTurn(t)
			mutate(&turn)
			engine := &adaptertest.Engine{}
			executor := a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}
			if err := executor.Check(ctx, turn.Sandbox.Verified, turn.Execution); !errors.Is(err, a.ErrUnsupported) {
				t.Fatalf("boundary check: %v", err)
			}
			result, err := (&a.TurnRunner{Executor: executor}).Run(ctx, turn)
			if !errors.Is(err, a.ErrUnsupported) || !result.IsError || len(engine.Prepared) != 0 {
				t.Fatalf("err=%v result=%+v prepared=%d", err, result, len(engine.Prepared))
			}
		})
	}
}

func TestDaggerEngineIsGrantedAndPolicyMustMatch(t *testing.T) {
	for _, address := range []string{"tcp://127.0.0.1:1234", "unix:///run/dagger/engine.sock", "docker-container://dagger-engine-v0.20.5"} {
		turn := daggerTurn(t)
		turn.Execution.Dagger.Engine = address
		engine := &adaptertest.Engine{}
		if _, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn); err != nil {
			t.Fatal(err)
		}
		req := engine.Requests[0]
		if req.Grants == nil || req.Grants.DaggerEngine != address || req.Profile.Dagger == nil || *req.Profile.Dagger != *turn.Execution.Dagger {
			t.Fatalf("grants %+v profile Dagger %+v", req.Grants, req.Profile.Dagger)
		}
	}
	for name, mutate := range map[string]func(*agent.Policy){
		"no engine":    func(p *agent.Policy) { p.DaggerEngine = "" },
		"other engine": func(p *agent.Policy) { p.DaggerEngine = "tcp://127.0.0.1:4321" },
	} {
		t.Run(name, func(t *testing.T) {
			turn := daggerTurn(t)
			engine := &adaptertest.Engine{Policy: mutate}
			_, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn)
			if !errors.Is(err, a.ErrUnsupported) || len(engine.Requests) != 0 || !engine.Released() {
				t.Fatalf("policy accepted: %v, launches=%d", err, len(engine.Requests))
			}
		})
	}
}

// Serial: the pinned runner's Docker forward inherits the process environment.
func TestMasonDockerEngineForwardUsesServiceHost(t *testing.T) {
	for _, host := range []string{"tcp://engine.example.ts.net:2375", "ssh://user@engine.example.ts.net", ""} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", host)
			if host == "" {
				if err := os.Unsetenv("DOCKER_HOST"); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("DOCKER_CONTEXT", "default")
			turn := daggerTurn(t)
			turn.Execution.Dagger.Engine = "docker-container://osmia-dagger-engine"
			turn.Sandbox.Verified.Environment[a.TokenEnvironment] = turn.SessionDirectory
			finish := filepath.Join(t.TempDir(), "finish")
			t.Cleanup(func() { _ = os.WriteFile(finish, nil, 0600) })
			// agenttest.Session cannot wait for a file signal and then succeed;
			// keep the agent alive until the Docker forward has been exercised.
			runner := sbxRunner(t, agenttest.Script(t, "fake-agent", fmt.Sprintf(`case "$*" in
*--version*) echo '2.1.200'; exit 0 ;;
*--help*) echo '--tools --strict-mcp-config'; exit 0 ;;
esac
cat >/dev/null
while [ ! -f %q ]; do sleep 0.01; done
echo '{"type":"result","subtype":"success","result":"built"}'
`, finish)))
			log := filepath.Join(t.TempDir(), "docker")
			runner.DockerBin = agenttest.Script(t, "docker", fmt.Sprintf("printf '%%s\\n%%s\\n' \"${DOCKER_HOST-}\" \"$*\" > %q\nexec cat\n", log))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			finished := make(chan error, 1)
			settled := make(chan struct{})
			defer func() { cancel(); <-settled }()
			go func() {
				result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: a.CoreEngine{Runner: runner}}}).Run(ctx, turn)
				if err == nil && (result.IsError || result.FinalResponse != "built") {
					err = fmt.Errorf("result %+v", result)
				}
				finished <- err
				close(settled)
			}()
			envPath := filepath.Join(turn.SessionDirectory, "sbx-exec-env.txt")
			var env, port string
			for port == "" {
				data, _ := os.ReadFile(envPath)
				env = string(data)
				for _, line := range strings.Split(env, "\n") {
					if p, ok := strings.CutPrefix(line, agent.EnvDaggerRunnerHost+"=tcp://host.docker.internal:"); ok {
						port = p
					}
				}
				select {
				case err := <-finished:
					t.Fatalf("session ended before engine connection: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			// The fake records the sbx client's environment. Only --env grants
			// enter a real sandbox; derive that environment from the runner's args.
			clientEnv := map[string]string{}
			for _, line := range strings.Split(env, "\n") {
				if name, value, ok := strings.Cut(line, "="); ok {
					clientEnv[name] = value
				}
			}
			args := strings.Split(readSbxFile(t, filepath.Join(turn.SessionDirectory, "sbx-exec-args.txt")), "\n")
			sandboxEnv := map[string]string{}
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--env" {
					name, value, explicit := strings.Cut(args[i+1], "=")
					if !explicit {
						value = clientEnv[name]
					}
					sandboxEnv[name] = value
					i++
				}
			}
			if _, ok := sandboxEnv["DOCKER_HOST"]; ok {
				t.Fatal("Docker host granted to sandbox")
			}
			if sandboxEnv[agent.EnvDaggerRunnerHost] != "tcp://host.docker.internal:"+port {
				t.Fatal("forwarded address not granted to sandbox")
			}
			if strings.Contains(strings.Join(args, "\n"), "docker.sock") {
				t.Fatal("Docker socket granted to sandbox")
			}
			conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			response := make([]byte, 4)
			if _, err := io.ReadFull(conn, response); err != nil || string(response) != "ping" {
				t.Fatalf("forward response %q: %v", response, err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != host+"\nexec -i osmia-dagger-engine buildctl dial-stdio\n" {
				t.Fatalf("Docker forward: %s", data)
			}
			if err := os.WriteFile(finish, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
		})
	}
}
