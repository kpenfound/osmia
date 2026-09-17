package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/agenttest/enforcertest"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
)

// launches is the process-launch seam every serve in these tests runs its
// role turns through: core's fake enforcer, which starts no process.
var launches = &launchEngine{}

// production is serve's own enforcement, which TestMain replaces.
var production = enforcement

func TestMain(m *testing.M) {
	enforcement = func() service.Enforcement {
		e := production()
		e.Engine = launches
		return e
	}
	os.Exit(m.Run())
}

// refusal is the reason the fake platform gives for a sandbox it cannot
// enforce.
const refusal = "this platform cannot hold the claude sandbox"

// launchEngine hands out core's fake enforcer for each role's sandbox. It
// records the turns its agent runs and refuses the claude sandbox the way a
// platform without a confiner does.
type launchEngine struct {
	mu    sync.Mutex
	turns []string
}

func (e *launchEngine) Enforcer(settings coreadapter.ExecutionSettings) (agent.Enforcer, error) {
	f := &enforcertest.Enforcer{Sandbox: settings.Mode, Image: settings.Image,
		Agent: func(_ context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
			e.mu.Lock()
			e.turns = append(e.turns, turn.Request.Name)
			e.mu.Unlock()
			return &agent.Result{ClaudeID: "session-" + turn.Request.Name, ResultText: "Noted", NumTurns: 1}, nil
		}}
	if settings.Mode == agent.SandboxClaude {
		f.PrepareErr = fmt.Errorf("%w: %s", agent.ErrUnsupported, refusal)
	}
	return f, nil
}

// await waits until the agent has run a turn named name.
func (e *launchEngine) await(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		e.mu.Lock()
		ran := slices.Contains(e.turns, name)
		e.mu.Unlock()
		if ran {
			return
		}
		if time.Now().After(deadline) {
			e.mu.Lock()
			defer e.mu.Unlock()
			t.Fatalf("turn %s did not run; ran %v", name, e.turns)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *launchEngine) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turns = nil
}

func (e *launchEngine) ran(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Contains(e.turns, name)
}

// serve runs osmia serve on root until the test ends.
func serve(t *testing.T, root string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan int, 1)
	var diag bytes.Buffer
	go func() { result <- Run(ctx, []string{"serve", "--root", root}, strings.NewReader(""), &bytes.Buffer{}, &diag) }()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-result:
			if code != 0 {
				t.Errorf("serve exited %d: %s", code, diag.String())
			}
		case <-time.After(time.Minute):
			t.Error("serve did not stop")
		}
	})
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := c.Health(context.Background()); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("serve did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeRunsRoleTurns shows a service started by osmia serve running role
// turns through the enforcers it builds: the librarian extracts an added
// project, the architect drafts a handed workstream and a message to the
// chief of staff is dispatched. A role whose sandbox the platform cannot
// enforce fails its turns with core's reason and the service keeps running.
func TestServeRunsRoleTurns(t *testing.T) {
	for _, sandbox := range []string{agent.SandboxNone, agent.SandboxClaude} {
		t.Run(sandbox, func(t *testing.T) {
			launches.reset()
			opts, clone := emptyFixture(t)
			root := opts.Config.Root
			f, err := os.OpenFile(filepath.Join(root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
			must(t, err)
			_, err = fmt.Fprintf(f, "[roles.librarian]\nsandbox=%q\n", sandbox)
			must(t, errors.Join(err, f.Close()))
			serve(t, root)

			var added service.ProjectResponse
			must(t, json.Unmarshal([]byte(successful(t, root, "project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", clone, "--json")), &added))
			extraction := awaitExtraction(t, root)
			if sandbox == agent.SandboxNone {
				launches.await(t, "extract-1-1")
			} else if extraction.State != "failed" || !strings.Contains(extraction.Reason, refusal) || launches.ran("extract-1-1") {
				t.Fatalf("refused librarian: %+v", extraction)
			}

			must(t, os.WriteFile(added.Project.Charter, []byte("1. Keep changes small.\n"), 0600))
			design := filepath.Join(filepath.Dir(clone), "design.md")
			must(t, os.WriteFile(design, []byte("# Design\n"), 0600))
			var handed service.HandInResponse
			must(t, json.Unmarshal([]byte(successful(t, root, "handin", string(added.Project.ID), design, "--json")), &handed))
			launches.await(t, "draft-1-1")

			var sent service.ConversationEntry
			must(t, json.Unmarshal([]byte(successful(t, root, "send", string(handed.Workstream), "Start with uploads.", "--json")), &sent))
			launches.await(t, sent.Turn)
		})
	}
}

// Without an injected fake, serve runs its turns through core's enforcers
// and MCP host.
func TestServeBuildsCoreEnforcement(t *testing.T) {
	e := production()
	if !reflect.DeepEqual(e.Engine, coreadapter.CoreEngine{}) {
		t.Fatalf("engine %#v", e.Engine)
	}
	host, ok := e.Hosts.(*coreadapter.MCPHost)
	if !ok || !reflect.DeepEqual(host.Transport, coreadapter.CoreTransport{}) || host.Container == nil {
		t.Fatalf("hosts %#v", e.Hosts)
	}
	opts := service.Enforce(service.Options{}, e)
	if opts.Threads == nil || opts.Librarian == nil || opts.Architect == nil {
		t.Fatalf("options %+v", opts)
	}
	for _, role := range []any{*opts.Librarian, *opts.Architect} {
		if !reflect.DeepEqual(role, service.Librarian(e)) && !reflect.DeepEqual(role, service.Architect(e)) {
			t.Fatalf("role enforcement %#v", role)
		}
	}
}
