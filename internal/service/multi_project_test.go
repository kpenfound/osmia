package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	otherProject config.ProjectID    = "p_fedcba9876543210fedcba9876543210"
	otherStream  config.WorkstreamID = "w_fedcba9876543210fedcba9876543210"
)

// twoProjectFixture is a root with two active projects that share one mason
// slot, each with its own trace, workstream and mason thread.
type twoProjectFixture struct {
	opts    Options
	cfg     *config.Config
	clock   *demoClock
	turns   *blockingTurns
	streams map[config.ProjectID]config.WorkstreamID
	mu      sync.Mutex
	live    map[config.ProjectID]*trace.Repository
}

func newTwoProjectFixture(t *testing.T) *twoProjectFixture {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "mp-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	root := opts.Config.Root
	top, err := os.ReadFile(filepath.Join(root, "config.toml"))
	must(t, err)
	listed := strings.Replace(string(top), fmt.Sprintf("active_projects = [%q]", project), fmt.Sprintf("active_projects = [%q, %q]", project, otherProject), 1)
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(listed+"[capacity]\nmasons = 1\n"), 0600))
	must(t, os.MkdirAll(filepath.Join(root, "projects", string(otherProject)), 0700))
	must(t, os.WriteFile(filepath.Join(root, "projects", string(otherProject), "config.toml"), []byte(`version = 1
upstream = "upstream/other"
fork = "owner/other"
clone = "`+filepath.Join(home, "other")+`"
`), 0600))
	opts.Workstreams = nil
	cfg, err := config.Load(opts.Config)
	must(t, err)
	f := &twoProjectFixture{opts: opts, cfg: cfg, clock: &demoClock{now: demoStart}, live: map[config.ProjectID]*trace.Repository{},
		streams: map[config.ProjectID]config.WorkstreamID{project: stream, otherProject: otherStream}}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	for _, p := range cfg.Projects {
		must(t, os.MkdirAll(p.Clone, 0700))
		repo, err := trace.Create(ctx, cfg.Root, p, f.clock.Now(), owner)
		must(t, err)
		ws := f.streams[p.ID]
		must(t, repo.CreateWorkstream(ctx, ws, f.clock.Now(), owner))
		must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: p.ID, Workstream: ws, At: f.clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}))
		f.queue(t, repo, p.ID, "first")
		must(t, repo.Close())
	}
	reply := adaptertest.Reply[coreadapter.SessionResult]{Value: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session"}, FinalResponse: "Done"}}
	f.turns = &blockingTurns{fake: &adaptertest.Turns{Script: *adaptertest.NewScript[coreadapter.PreparedTurn](reply, reply, reply, reply, reply, reply)}, hooks: map[string]func(context.Context){}}
	f.opts.Threads = func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		if cfg.Project.ID != r.Project() {
			return nil, fmt.Errorf("project %s bound with the configuration of %q", r.Project(), cfg.Project.ID)
		}
		f.mu.Lock()
		f.live[r.Project()] = r
		f.mu.Unlock()
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: f.turns, Now: f.clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(root, "sessions", string(r.Project()), in.Agent, in.Turn)}, nil
			}}, nil
	}
	f.opts.Reconciliation.Now = f.clock.Now
	return f
}

// queue accepts an owner message to the project's mason thread.
func (f *twoProjectFixture) queue(t *testing.T, repo *trace.Repository, id config.ProjectID, turn string) {
	t.Helper()
	turn = string(id[len(id)-4:]) + "-" + turn
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: id, Workstream: f.streams[id], At: f.clock.Now(), Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "message_" + turn, Depth: 1},
		AgentID: demoAgent, ThreadID: demoThread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: " + turn}
	_, err := repo.EnqueueTurn(context.Background(), req)
	must(t, err)
}

// turn is the ID queue gives a project's turn.
func turn(id config.ProjectID, name string) string { return string(id[len(id)-4:]) + "-" + name }

// block makes the named turn signal entered and wait for release or for its
// context to end.
func (f *twoProjectFixture) block(id config.ProjectID, name string) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	f.turns.mu.Lock()
	f.turns.hooks[turn(id, name)] = func(ctx context.Context) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	f.turns.mu.Unlock()
	return entered, release
}

func (f *twoProjectFixture) repository(id config.ProjectID) *trace.Repository {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[id]
}

// dispatchedTurns lists the turns the project's trace has turn operations for.
func (f *twoProjectFixture) dispatchedTurns(t *testing.T, repo *trace.Repository, id config.ProjectID) []string {
	t.Helper()
	ops, err := repo.Operations(f.streams[id])
	must(t, err)
	var out []string
	for _, op := range ops {
		if in, err := thread.DecodeTurn(op.Operation); err == nil {
			out = append(out, in.Turn)
		}
	}
	slices.Sort(out)
	return out
}

func await(t *testing.T, what string, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(demoTimeout):
		t.Fatalf("%s did not happen", what)
	}
}

func TestSeveralProjectsShareCapacityPauseApartAndResumeFromTheirOwnTraces(t *testing.T) {
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	// The first project starts paused, so the other project's turn takes the
	// only mason slot.
	store, _, err := runtime.Open(runtime.Inputs{Config: f.cfg})
	must(t, err)
	must(t, store.SetPause(runtime.Pause{Target: runtime.Target{Scope: "project", Project: project}, Mode: "soft", Source: runtime.PauseOwner, Reason: "Owner pause"}))
	must(t, store.Close())
	otherEntered, otherRelease := f.block(otherProject, "first")
	entered, _ := f.block(project, "first")

	s, err := Start(ctx, f.opts)
	must(t, err)
	stopped := false
	defer func() {
		if !stopped {
			s.Close()
		}
	}()
	if got := s.current().ProjectIDs(); !slices.Equal(got, []config.ProjectID{project, otherProject}) {
		t.Fatalf("active projects %v", got)
	}
	await(t, "the other project's turn", otherEntered)
	paused, other := f.repository(project), f.repository(otherProject)
	if paused == nil || other == nil || paused == other {
		t.Fatalf("each project needs its own trace: %v %v", paused, other)
	}
	// The pause holds only its own project: its turn is neither dispatched nor
	// waiting for a slot.
	soon(t, "the other project's turn holds the only mason slot", func() bool {
		st, d := s.capacityStatus(nil)
		return d == nil && st.Roles[0].Used == 1 && len(st.Roles[0].Waiting) == 0
	})
	if got := f.dispatchedTurns(t, paused, project); len(got) != 0 {
		t.Fatalf("paused project dispatched %v", got)
	}

	// Resumed, the first project's turn waits for the slot the other project
	// holds.
	must(t, s.store.ClearPause(runtime.Target{Scope: "project", Project: project}, runtime.PauseOwner))
	soon(t, "the first project's turn waits for the shared mason slot", func() bool {
		st, d := s.capacityStatus(nil)
		if d != nil || st.Roles[0].Used != 1 || len(st.Roles[0].Waiting) != 1 {
			return false
		}
		w := st.Roles[0].Waiting[0]
		return w.Workstream == stream && w.Turn == turn(project, "first") && w.Reason == "capacity"
	})
	// Several passes of both loops run while the slot stays taken.
	time.Sleep(2500 * time.Millisecond)
	if got := f.dispatchedTurns(t, paused, project); len(got) != 0 {
		t.Fatalf("first project dispatched %v past the shared capacity", got)
	}

	// Once the other project's turn completes, the slot goes to the first.
	close(otherRelease)
	await(t, "the first project's turn", entered)
	f.queue(t, other, otherProject, "second")
	must(t, s.Close())
	stopped = true

	// The first project's turn was in flight at the stop; after the restart
	// each project resumes from its own trace.
	s, err = Start(ctx, f.opts)
	must(t, err)
	stopped = false
	for _, id := range []config.ProjectID{project, otherProject} {
		awaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
			ops, err := f.repository(id).Operations(f.streams[id])
			must(t, err)
			want := 1
			if id == otherProject {
				want = 2
			}
			if len(ops) != want {
				return nil
			}
			return ops
		})
	}
	must(t, s.Close())
	stopped = true
	for _, p := range f.cfg.Projects {
		repo, err := trace.Open(f.cfg.Root, p)
		must(t, err)
		th, err := repo.Thread(f.streams[p.ID], demoAgent)
		must(t, err)
		want := []string{turn(p.ID, "first")}
		if p.ID == otherProject {
			want = append(want, turn(p.ID, "second"))
		}
		if got := f.dispatchedTurns(t, repo, p.ID); !slices.Equal(got, want) {
			t.Fatalf("project %s dispatched %v, want %v", p.ID, got, want)
		}
		for _, q := range th.Turns {
			if q.CompletedAt.IsZero() || q.Request.Project != p.ID {
				t.Fatalf("project %s turn %s: %+v", p.ID, q.Request.TurnID, q)
			}
		}
		must(t, repo.Close())
	}
}

func TestProjectScopedRequestsNameOneOfSeveralProjects(t *testing.T) {
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	s, c := start(t, f.opts)
	text := "Build it"
	both := string(project) + ", " + string(otherProject)
	_, err := c.HandIn(ctx, HandInRequest{Key: "k", Stdin: &text})
	var api *APIError
	if !errors.As(err, &api) || api.Code != Validation || !strings.Contains(api.Message, "several projects are active; name one of "+both) {
		t.Fatalf("hand-in without a project: %v", err)
	}
	unknown := config.ProjectID("p_00000000000000000000000000000000")
	for name, call := range map[string]func() error{
		"extract": func() error { _, err := c.ExtractProject(ctx, ""); return err },
		"rebase":  func() error { _, err := c.RebaseProject(ctx, ""); return err },
		"answer":  func() error { _, err := c.Answer(ctx, 1, "yes"); return err },
	} {
		if err := call(); !errors.As(err, &api) || api.Code != Validation || !strings.Contains(api.Message, both) {
			t.Fatalf("%s without a project: %v", name, err)
		}
	}
	if _, err := c.RebaseProject(ctx, unknown); !errors.As(err, &api) || api.Code != NotFound || !strings.Contains(api.Message, "the active projects are "+both) {
		t.Fatalf("rebase of an inactive project: %v", err)
	}
	// A named project is acted on alone.
	out, err := c.RebaseProject(ctx, otherProject)
	must(t, err)
	if out.Project != otherProject || len(out.Covered)+len(out.Skipped) != 1 || len(out.Skipped) == 1 && out.Skipped[0].Workstream != otherStream {
		t.Fatalf("rebase of the other project: %+v", out)
	}
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Project != nil || len(cfg.Projects) != 2 || cfg.Projects[0].ID != project || cfg.Projects[1].ID != otherProject {
		t.Fatalf("configuration projects: %+v %+v", cfg.Project, cfg.Projects)
	}
	status, err := c.Statuses(ctx)
	must(t, err)
	var listed []config.ProjectID
	for _, w := range status.Workstreams {
		listed = append(listed, w.Project)
	}
	if !slices.Equal(listed, []config.ProjectID{project, otherProject}) {
		t.Fatalf("status lists workstreams of %v", listed)
	}
	// A workstream is found in whichever project holds it.
	if _, api := s.workstreamStatus(string(otherStream)); api != nil {
		t.Fatalf("status of the other project's workstream: %v", api)
	}
	if p, ws, repo, api := s.conversationTrace(string(otherStream)); api != nil || p != otherProject || ws != otherStream || repo.Project() != otherProject {
		t.Fatalf("conversation of the other project's workstream: %v %v %v", p, ws, api)
	}
}
