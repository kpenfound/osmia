package client

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest/enforcertest"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/fleet"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// engine is the fake agent and sandbox engine: core's own fake enforcer, which
// checks every grant and request with core's code and launches no process,
// running a test's script as the session.
type engine struct {
	script  func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error)
	dirs    []string
	plugins map[string]string
}

func (e *engine) Enforcer(settings coreadapter.ExecutionSettings) (agent.Enforcer, error) {
	return &enforcertest.Enforcer{Sandbox: settings.Mode, Image: settings.Image, SandboxAgent: settings.Agent,
		Agent: func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
			if e.script != nil {
				return e.script(ctx, turn)
			}
			return &agent.Result{ClaudeID: "session-" + turn.Request.Name, ResultText: "done", SessionDir: turn.Request.SessionDir}, nil
		}}, nil
}

func (e *engine) SkillDirs() []string { return e.dirs }

func (e *engine) PrepareSkills(_ context.Context, refs []string) ([]string, error) {
	var out []string
	for _, ref := range refs {
		dir, ok := e.plugins[ref]
		if !ok {
			return nil, errors.New("skill " + ref + " is not prepared")
		}
		out = append(out, dir)
	}
	return out, nil
}

func finished(turn *enforcertest.Turn) (*agent.Result, error) {
	return &agent.Result{ClaudeID: "session-" + turn.Request.Name, ResultText: "done", SessionDir: turn.Request.SessionDir}, nil
}

// rig is a client under test, its fake server and its fake tools.
type rig struct {
	t          *testing.T
	srv        *server
	repo       *repository
	dir        string
	engine     *engine
	logs       *syncBuffer
	interrupts chan struct{}
	haveMu     sync.Mutex
	have       map[string]bool
	env        map[string]string
	slots      int

	client *Client
	done   chan error
	cancel context.CancelFunc
}

func newRig(t *testing.T, options fleet.Options) *rig {
	t.Helper()
	repo := newRepository(t)
	put(t, repo.dir, "src/main.go", "package main\n")
	put(t, repo.dir, "src/nested/a.txt", "a\n")
	put(t, repo.dir, "docs/notes.md", "# notes\n")
	must(t, os.Chmod(filepath.Join(repo.dir, "src/main.go"), 0o755))
	must(t, os.Symlink("main.go", filepath.Join(repo.dir, "src/link")))
	repo.commit("first")
	if options.Grace == 0 {
		options.Grace = 3 * time.Second
	}
	if options.Heartbeat == 0 {
		options.Heartbeat = time.Second
	}
	if options.Poll == 0 {
		options.Poll = time.Second
	}
	r := &rig{t: t, srv: newServer(t, repo.dir, options), repo: repo, dir: t.TempDir(), engine: &engine{}, logs: &syncBuffer{}, interrupts: make(chan struct{}, 4),
		have: map[string]bool{"claude": true, "git": true}, env: map[string]string{}, slots: 2}
	t.Cleanup(func() {
		r.stop()
		if t.Failed() {
			t.Log(r.logs.String())
		}
	})
	return r
}

func (r *rig) options() Options {
	return Options{Server: r.srv.http.URL, Slots: r.slots, Dir: r.dir, Interrupts: r.interrupts, Logger: slog.New(slog.NewTextHandler(r.logs, nil)),
		Engine: r.engine, RetryEvery: 200 * time.Millisecond, ExitWait: 5 * time.Second,
		LookPath: func(name string) (string, error) {
			r.haveMu.Lock()
			defer r.haveMu.Unlock()
			if r.have[name] {
				return "/fake/" + name, nil
			}
			return "", errors.New(name + " is not installed")
		},
		Command: func(_ context.Context, path string, _ ...string) (string, error) {
			return filepath.Base(path) + " 1.0\n", nil
		},
		Getenv: func(name string) string { return r.env[name] }}
}

// start runs the client until stop, or until it exits by itself.
func (r *rig) start() {
	r.t.Helper()
	c, err := newClient(r.options())
	must(r.t, err)
	ctx, cancel := context.WithCancel(context.Background())
	r.client, r.cancel, r.done = c, cancel, make(chan error, 1)
	go func() { r.done <- c.run(ctx) }()
}

func (r *rig) stop() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		r.t.Error("the client did not stop")
	}
	r.cancel = nil
}

// install makes a tool the machine has.
func (r *rig) install(name string) {
	r.haveMu.Lock()
	defer r.haveMu.Unlock()
	r.have[name] = true
}

func (r *rig) connected() bool {
	_, ok := r.srv.reg().Client(node.ID)
	return ok
}

func (r *rig) waitConnected() {
	r.t.Helper()
	waitFor(r.t, "the client to connect", r.connected)
}

func (r *rig) waitExit() error {
	r.t.Helper()
	select {
	case err := <-r.done:
		r.cancel = nil
		return err
	case <-time.After(20 * time.Second):
		r.t.Fatal("the client did not exit")
		return nil
	}
}

func (r *rig) running() int {
	r.client.mu.Lock()
	defer r.client.mu.Unlock()
	return len(r.client.running)
}

func (r *rig) pendingTurns() []string {
	r.client.mu.Lock()
	defer r.client.mu.Unlock()
	var out []string
	for id := range r.client.pending {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func handout(revision string, profiles ...api.ProfileSpec) api.ProfileHandout {
	h := api.ProfileHandout{Revision: revision, Profiles: profiles, Roles: []api.RoleRequirement{}}
	for _, p := range profiles {
		h.Roles = append(h.Roles, api.RoleRequirement{Role: "role-" + p.Name, Profile: p.Name, Chain: []string{p.Name}, Backend: p.Backend, Model: p.Model, Sandbox: "none", Domains: []string{}, Skills: []string{}})
	}
	return h
}

// offer is a mason turn on the project's first commit, with the view built from
// src, as the server offers one.
func (r *rig) offer(turn string, change ...func(*api.TurnOffer)) api.TurnOffer {
	o := api.TurnOffer{Turn: turn, Project: string(project), Workstream: "w_00000000000000000000000000000001", Thread: "thread-" + turn, Role: "mason", Profile: "main",
		Settings:     coreadapter.Profile{Name: "main", Backend: "claude", Model: "model"},
		Capabilities: coreadapter.Capabilities{WriteFiles: true, Execute: true, Tools: []string{"file_read", "file_write"}},
		Execution:    coreadapter.ExecutionSettings{Mode: "none"}, ToolEndpoint: api.MCPPath(turn), ToolToken: "token-" + turn, ReturnView: true,
		View: api.TurnView{Kind: api.ViewGit, Project: string(project), Commit: r.repo.git("rev-parse", "HEAD"), Paths: []string{"src"}}}
	for _, f := range change {
		f(&o)
	}
	return o
}

// give offers the turn to the client and returns the client's claim.
func (r *rig) give(o api.TurnOffer) fleet.Claim {
	r.t.Helper()
	r.srv.register(o.Turn, o.ToolToken, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	claim, err := r.srv.reg().Offer(ctx, node.ID, o)
	must(r.t, err)
	return claim
}

func (r *rig) waitResult(turn string) api.TurnResult {
	r.t.Helper()
	waitFor(r.t, "the result of "+turn, func() bool { return len(r.srv.resultsOf(turn)) > 0 })
	return r.srv.resultsOf(turn)[0]
}

func (r *rig) viewDirOf(turn *enforcertest.Turn) string { return turn.Request.Workspace.Directory() }
