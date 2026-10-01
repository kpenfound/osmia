package coreadapter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/agenttest"
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
			runner := sbxRunner(t, `case "$*" in
*--version*) echo '2.1.200'; exit 0 ;;
*--help*) echo '--tools --strict-mcp-config'; exit 0 ;;
esac
cat >/dev/null
echo '{"type":"result","subtype":"success","result":"built"}'
`)
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
