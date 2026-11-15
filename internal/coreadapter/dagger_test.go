package coreadapter_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	a "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
)

// daggerTurn is a mason turn in sbx that may execute commands and asks for
// the Dagger CLI against an engine on the host's loopback. Its view holds the
// empty repository the service creates for such a turn.
func daggerTurn(t *testing.T) a.PreparedTurn {
	t.Helper()
	turn := toolTurn(t, "sbx")
	turn.Scope.Role = "mason"
	turn.Profile.Timeout = time.Minute
	turn.Sandbox.Verified.Workspace.Access = a.ReadWrite
	turn.Sandbox.Verified.Capabilities.WriteFiles = true
	turn.Sandbox.Verified.Capabilities.Execute = true
	turn.Execution.Dagger = &agent.Dagger{Engine: "tcp://127.0.0.1:1234", Version: "v0.20.5"}
	emptyRepository(t, turn.Sandbox.Verified.Workspace.Directory)
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

// cloudTurn is daggerTurn on Dagger Cloud: no host engine, the token's
// variable named, and the variables the service adds for it in the turn's
// environment.
func cloudTurn(t *testing.T, token string) a.PreparedTurn {
	t.Helper()
	turn := daggerTurn(t)
	turn.Execution.Dagger = nil
	turn.Execution.DaggerCloudTokenEnv = "OSMIA_TEST_DAGGER_CLOUD_TOKEN"
	turn.Sandbox.Verified.Environment[a.DaggerEngineEnvironment] = "cloud"
	turn.Sandbox.Verified.Environment[a.DaggerCloudTokenEnvironment] = token
	return turn
}

// A mason turn on Dagger Cloud reaches its sandbox with DAGGER_ENGINE=cloud
// and the token, both by name, and an empty repository at its view's root.
// agentcore gives it no Dagger: no CLI install, engine forward or host port.
func TestSbxMasonTurnOnDaggerCloud(t *testing.T) {
	const token = "mason-cloud-token"
	turn := cloudTurn(t, token)
	turn.Sandbox.Verified.Environment[a.TokenEnvironment] = turn.SessionDirectory
	runner := sbxRunner(t, agenttest.Session(t, agent.AgentClaude, agenttest.Turn{ResultText: "built"}).Bin)
	dir := filepath.Dir(runner.SbxBin)
	result, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: a.CoreEngine{Runner: runner}}}).Run(context.Background(), turn)
	if err != nil || result.IsError || result.FinalResponse != "built" {
		t.Fatalf("result %+v: %v", result, err)
	}
	env := strings.Split(readSbxFile(t, filepath.Join(turn.SessionDirectory, "sbx-exec-env.txt")), "\n")
	for _, want := range []string{a.DaggerEngineEnvironment + "=cloud", a.DaggerCloudTokenEnvironment + "=" + token} {
		if !slices.Contains(env, want) {
			t.Fatalf("session environment lacks %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	for _, line := range env {
		if strings.HasPrefix(line, agent.EnvDaggerRunnerHost+"=") {
			t.Fatalf("a Dagger Cloud turn was given a host engine: %s", line)
		}
	}
	args := readSbxFile(t, filepath.Join(turn.SessionDirectory, "sbx-exec-args.txt"))
	if strings.Contains(args, token) {
		t.Fatal("the Dagger Cloud token is on the sbx command line")
	}
	for _, name := range []string{a.DaggerEngineEnvironment, a.DaggerCloudTokenEnvironment} {
		if !strings.Contains(args, "--env\n"+name+"\n") {
			t.Fatalf("sbx exec does not pass %s by name:\n%s", name, args)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "sbx-setup.txt")); err == nil {
		t.Fatal("a Dagger Cloud turn ran a Dagger CLI install")
	}
	policy, _ := os.ReadFile(filepath.Join(dir, "sbx-policy.txt"))
	for _, line := range strings.Split(string(policy), "\n") {
		// The turn's MCP server at 127.0.0.1:1 is the only host port.
		if strings.Contains(line, "allow network") && !strings.HasSuffix(line, " localhost:1") {
			t.Fatalf("a Dagger Cloud turn was allowed a host port: %s", line)
		}
	}
}

// A Dagger Cloud turn is granted its variables and the view's empty
// repository, and asks agentcore for no Dagger engine.
func TestDaggerCloudTurnGrants(t *testing.T) {
	turn := cloudTurn(t, "mason-cloud-token")
	engine := &adaptertest.Engine{}
	if _, err := (&a.TurnRunner{Executor: a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}}).Run(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	req := engine.Requests[0]
	if req.Grants == nil || req.Grants.DaggerEngine != "" || req.Profile.Dagger != nil || !req.Grants.Mounts[0].EmptyRepository {
		t.Fatalf("grants %+v profile Dagger %+v", req.Grants, req.Profile.Dagger)
	}
	for _, name := range []string{a.DaggerEngineEnvironment, a.DaggerCloudTokenEnvironment} {
		if !slices.Contains(req.Grants.Env, name) || req.Env[name] == "" {
			t.Fatalf("%s is not granted: %v", name, req.Grants.Env)
		}
	}
}

// The Dagger Cloud variables reach only a Dagger Cloud turn, and a Dagger
// Cloud turn only with them, in sbx, with execution, and without a host
// engine besides.
func TestDaggerCloudTurnBoundary(t *testing.T) {
	ctx := context.Background()
	for name, mutate := range map[string]func(*a.PreparedTurn){
		"variables without Dagger Cloud": func(turn *a.PreparedTurn) { turn.Execution.DaggerCloudTokenEnv = "" },
		"no token": func(turn *a.PreparedTurn) {
			delete(turn.Sandbox.Verified.Environment, a.DaggerCloudTokenEnvironment)
		},
		"another engine": func(turn *a.PreparedTurn) {
			turn.Sandbox.Verified.Environment[a.DaggerEngineEnvironment] = "tcp://127.0.0.1:1234"
		},
		"and a host engine": func(turn *a.PreparedTurn) {
			turn.Execution.Dagger = &agent.Dagger{Engine: "tcp://127.0.0.1:1234", Version: "v0.20.5"}
		},
		"container": func(turn *a.PreparedTurn) {
			turn.Execution.Mode, turn.Execution.Image = "container", "fixture-image"
		},
		"no execute":     func(turn *a.PreparedTurn) { turn.Sandbox.Verified.Capabilities.Execute = false },
		"bad token name": func(turn *a.PreparedTurn) { turn.Execution.DaggerCloudTokenEnv = "A;B" },
	} {
		t.Run(name, func(t *testing.T) {
			turn := cloudTurn(t, "mason-cloud-token")
			mutate(&turn)
			engine := &adaptertest.Engine{}
			executor := a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}
			if err := executor.Check(ctx, turn.Sandbox.Verified, turn.Execution); !errors.Is(err, a.ErrUnsupported) {
				t.Fatalf("boundary check: %v", err)
			}
			if len(engine.Prepared) != 0 {
				t.Fatal("a refused Dagger Cloud turn was prepared")
			}
		})
	}
}

// DaggerCloudEnvironment reads the token from the variable a Dagger Cloud
// turn names, refuses an unset one, and adds nothing to any other turn.
func TestDaggerCloudEnvironment(t *testing.T) {
	getenv := func(name string) string { return map[string]string{"SET": "token-value"}[name] }
	env, err := a.DaggerCloudEnvironment(a.ExecutionSettings{DaggerCloudTokenEnv: "SET"}, getenv)
	if err != nil || len(env) != 2 || env[a.DaggerEngineEnvironment] != "cloud" || env[a.DaggerCloudTokenEnvironment] != "token-value" {
		t.Fatalf("environment %v: %v", env, err)
	}
	if _, err := a.DaggerCloudEnvironment(a.ExecutionSettings{DaggerCloudTokenEnv: "UNSET"}, getenv); err == nil || !strings.Contains(err.Error(), "UNSET") {
		t.Fatalf("unset token: %v", err)
	}
	if env, err := a.DaggerCloudEnvironment(a.ExecutionSettings{Dagger: &agent.Dagger{Engine: "tcp://127.0.0.1:1234"}}, getenv); err != nil || env != nil {
		t.Fatalf("host engine environment %v: %v", env, err)
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

// A Dagger turn's view may hold a Git repository at its root, a directory of
// regular files. Any other VCS metadata in it, and any repository in a view
// for a turn without Dagger, is refused before a session is prepared.
func TestBoundaryAdmitsOnlyADaggerViewsRootRepository(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		prepare func(t *testing.T, view string)
		dagger  bool
		allowed bool
	}{
		"empty repository": {dagger: true, allowed: true, prepare: emptyRepository},
		"repository file": {dagger: true, prepare: func(t *testing.T, view string) {
			mustWrite(t, filepath.Join(view, ".git"), "gitdir: /private")
		}},
		"repository link": {dagger: true, prepare: func(t *testing.T, view string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(view, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
		"link inside the repository": {dagger: true, prepare: func(t *testing.T, view string) {
			mustWrite(t, filepath.Join(view, ".git", "HEAD"), "ref: refs/heads/main\n")
			if err := os.Symlink(t.TempDir(), filepath.Join(view, ".git", "hooks")); err != nil {
				t.Fatal(err)
			}
		}},
		"nested repository": {dagger: true, prepare: func(t *testing.T, view string) {
			mustWrite(t, filepath.Join(view, ".git", "HEAD"), "ref: refs/heads/main\n")
			mustWrite(t, filepath.Join(view, "sub", ".git", "HEAD"), "ref: refs/heads/main\n")
		}},
		"other metadata": {dagger: true, prepare: func(t *testing.T, view string) {
			mustWrite(t, filepath.Join(view, ".git", "HEAD"), "ref: refs/heads/main\n")
			mustWrite(t, filepath.Join(view, ".jj", "repo"), "x")
		}},
		"no Dagger": {prepare: func(t *testing.T, view string) {
			mustWrite(t, filepath.Join(view, ".git", "HEAD"), "ref: refs/heads/main\n")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			turn := daggerTurn(t)
			if !c.dagger {
				turn.Execution.Dagger = nil
			}
			view := turn.Sandbox.Verified.Workspace.Directory
			if err := os.RemoveAll(filepath.Join(view, ".git")); err != nil {
				t.Fatal(err)
			}
			c.prepare(t, view)
			engine := &adaptertest.Engine{}
			executor := a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}
			err := executor.Check(ctx, turn.Sandbox.Verified, turn.Execution)
			if c.allowed {
				if err != nil {
					t.Fatalf("boundary check: %v", err)
				}
				if _, err := (&a.TurnRunner{Executor: executor}).Run(ctx, turn); err != nil || len(engine.Requests) != 1 {
					t.Fatalf("turn: %v requests=%d", err, len(engine.Requests))
				}
				return
			}
			if !errors.Is(err, a.ErrUnsupported) {
				t.Fatalf("boundary check: %v", err)
			}
			result, err := (&a.TurnRunner{Executor: executor}).Run(ctx, turn)
			if !errors.Is(err, a.ErrUnsupported) || !result.IsError || len(engine.Prepared) != 0 {
				t.Fatalf("err=%v result=%+v prepared=%d", err, result, len(engine.Prepared))
			}
		})
	}
}

// emptyRepository leaves in view what `git init --quiet --template=` does.
func emptyRepository(t *testing.T, view string) {
	t.Helper()
	mustWrite(t, filepath.Join(view, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(view, ".git", "config"), "[core]\n\trepositoryformatversion = 0\n\tbare = false\n")
	for _, dir := range []string{"objects/info", "objects/pack", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(view, ".git", dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// A Dagger turn marks its view mount, and that mount alone, as holding only an
// empty repository; core admits the writable mount without granting version
// control. A turn without Dagger marks no mount.
func TestDaggerTurnMarksOnlyItsViewMountAsAnEmptyRepository(t *testing.T) {
	for name, dagger := range map[string]bool{"dagger": true, "no dagger": false} {
		t.Run(name, func(t *testing.T) {
			turn := daggerTurn(t)
			if !dagger {
				turn.Execution.Dagger = nil
				if err := os.RemoveAll(filepath.Join(turn.Sandbox.Verified.Workspace.Directory, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			engine := &adaptertest.Engine{}
			executor := a.CoreExecutor{Required: turn.Sandbox.Verified, Runner: engine}
			if _, err := (&a.TurnRunner{Executor: executor}).Run(context.Background(), turn); err != nil || len(engine.Requests) != 1 {
				t.Fatalf("turn: %v requests=%d", err, len(engine.Requests))
			}
			grants := engine.Requests[0].Grants
			if grants == nil || grants.VCS || len(grants.Mounts) < 2 {
				t.Fatalf("grants %+v", grants)
			}
			for i, mount := range grants.Mounts {
				if mount.EmptyRepository != (dagger && i == 0) {
					t.Fatalf("mount %d %+v, dagger=%v", i, mount, dagger)
				}
			}
			policy := engine.Prepared[0].Policy()
			if policy.VCS || policy.Mounts[0].EmptyRepository != dagger {
				t.Fatalf("policy VCS=%v mounts %+v", policy.VCS, policy.Mounts)
			}
			for _, name := range agent.VCSExecutables {
				if !slices.Contains(policy.DeniedExecutables, name) {
					t.Fatalf("VCS executable %s is not denied", name)
				}
			}
		})
	}
}

func mustWrite(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
