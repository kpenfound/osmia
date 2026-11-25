package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

var chiefBase = fixturekit.DemoStart

func chiefAt(n int) time.Time { return chiefBase.Add(time.Duration(n) * time.Second) }

type chiefAgent struct {
	mu       sync.Mutex
	gates    map[string]chan struct{}
	running  map[string]bool
	started  []string
	finished []string
	peak     int
	free     bool
}

func newChiefAgent() *chiefAgent {
	return &chiefAgent{gates: map[string]chan struct{}{}, running: map[string]bool{}}
}

func (a *chiefAgent) gate(turn string) chan struct{} {
	g := a.gates[turn]
	if g == nil {
		g = make(chan struct{})
		a.gates[turn] = g
		if a.free {
			close(g)
		}
	}
	return g
}

func (a *chiefAgent) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	turn := p.Scope.Turn + "@" + p.Scope.Workstream
	a.mu.Lock()
	a.running[turn] = true
	a.peak = max(a.peak, len(a.running))
	a.started = append(a.started, turn)
	gate := a.gate(turn)
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.running, turn)
		a.finished = append(a.finished, turn)
		a.mu.Unlock()
	}()
	select {
	case <-gate:
	case <-ctx.Done():
		return coreadapter.SessionResult{}, ctx.Err()
	}
	session := coreadapter.BackendSession{Backend: p.Profile.Backend, ID: "session"}
	switch p.Prompt {
	case "fail":
		return coreadapter.SessionResult{Session: session, FinalResponse: "partial", IsError: true}, nil
	case "cancel":
		return coreadapter.SessionResult{Session: session, FinalResponse: "stopped", Cancelled: true}, nil
	case "wait":
		return coreadapter.SessionResult{Session: session, FinalResponse: "Question", Outcome: &coreadapter.Outcome{Status: "waiting", Report: "Question"}}, nil
	}
	return coreadapter.SessionResult{Session: session, FinalResponse: "Answer to " + p.Prompt}, nil
}

func (a *chiefAgent) CheckResume(context.Context, coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
	return nil
}

func (a *chiefAgent) release(turns ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, turn := range turns {
		select {
		case <-a.gate(turn):
		default:
			close(a.gate(turn))
		}
	}
}

func (a *chiefAgent) releaseAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.free = true
	for _, g := range a.gates {
		select {
		case <-g:
		default:
			close(g)
		}
	}
}

func (a *chiefAgent) snapshot() (running, started, finished []string, peak int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for turn := range a.running {
		running = append(running, turn)
	}
	slices.Sort(running)
	return running, slices.Clone(a.started), slices.Clone(a.finished), a.peak
}

func (a *chiefAgent) hasStarted(turn string) bool {
	_, started, _, _ := a.snapshot()
	return slices.Contains(started, turn)
}

func (a *chiefAgent) runningCount() int {
	running, _, _, _ := a.snapshot()
	return len(running)
}

func chiefThreads(root string, now func() time.Time, agent *chiefAgent) func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error) {
	return func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: agent, Now: now, ResumableRoles: func(string) bool { return true }},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				dir := filepath.Join(root, "sessions", string(in.Workstream), in.Agent, in.Turn)
				return coreadapter.PreparedTurn{SessionDirectory: dir}, os.MkdirAll(dir, 0700)
			}}, nil
	}
}

type chiefFleet struct {
	opts    Options
	cfg     *config.Config
	ids     []config.ProjectID
	streams []config.WorkstreamID
	agent   *chiefAgent
}

func newChiefFleet(t *testing.T, projects int, topConfig, projectConfig string) *chiefFleet {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "chief-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	root := filepath.Join(home, "root")
	fixturekit.Must(t, os.MkdirAll(root, 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("version = 1\nworkspaces = \"git\"\n[events]\nwindow = \"50ms\"\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n[profiles.other]\nagent = \"codex\"\nmodel = \"other\"\n"+topConfig), 0600))
	f := &chiefFleet{agent: newChiefAgent()}
	for i := 0; i < projects; i++ {
		id := config.ProjectID(fmt.Sprintf("p_%032x", i+1))
		f.ids = append(f.ids, id)
		f.streams = append(f.streams, config.WorkstreamID(fmt.Sprintf("w_%032x", i+1)))
		fixturekit.RegisterProject(t, root, id, fmt.Sprintf("version = 1\nupstream = \"upstream/repo%d\"\nfork = \"owner/repo%d\"\nclone = %q\n%s", i, i, filepath.Join(home, fmt.Sprintf("clone%d", i)), projectConfig))
	}
	f.cfg, err = projectstore.LoadConfig(config.Options{Root: root})
	fixturekit.Must(t, err)
	for i, p := range f.cfg.Projects {
		fixturekit.Must(t, os.MkdirAll(p.Clone, 0700))
		fixturekit.DemoGit(t, home, "-C", p.Clone, "init", "-q")
		repo, err := trace.Create(ctx, f.cfg.Root, p, chiefBase, ownerActor)
		fixturekit.Must(t, err)
		fixturekit.Must(t, repo.CreateWorkstream(ctx, f.streams[i], chiefBase, ownerActor))
		_, err = repo.EnsureChiefOfStaff(ctx, f.streams[i], chiefBase, serviceActor)
		fixturekit.Must(t, err)
		fixturekit.Must(t, repo.Close())
	}
	f.opts = testOptions(config.Options{Root: root})
	f.opts.Workstreams = nil
	f.opts.Reconciliation.Now = func() time.Time { return time.Now().UTC() }
	f.opts.Threads = chiefThreads(root, f.opts.Reconciliation.Now, f.agent)
	return f
}

func (f *chiefFleet) key(i int, turn string) string { return turn + "@" + string(f.streams[i]) }

func (f *chiefFleet) repo(t *testing.T, s *Service, i int) *trace.Repository {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.projects {
		if p.id == f.ids[i] {
			return p.repository
		}
	}
	t.Fatalf("project %d is not running", i)
	return nil
}

func (f *chiefFleet) request(i int, turn string, at time.Time, actor trace.Actor, cause, prompt, profile string) trace.TurnRequest {
	p, err := f.cfg.NamedProfile(profile)
	if err != nil {
		panic(err)
	}
	return trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, Revision: 1, ID: "request_" + turn, Project: f.ids[i], Workstream: f.streams[i], At: at, Actor: actor, Cause: cause},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: turn, Profile: p, Prompt: prompt}
}

func (f *chiefFleet) background(t *testing.T, s *Service, i int, turn string, at int) {
	t.Helper()
	f.backgroundWith(t, s, i, turn, at, "ok", "default")
}

func (f *chiefFleet) backgroundWith(t *testing.T, s *Service, i int, turn string, at int, prompt, profile string) {
	t.Helper()
	_, err := f.repo(t, s, i).EnqueueTurn(context.Background(), f.request(i, turn, chiefAt(at), serviceActor, "event", prompt, profile))
	fixturekit.Must(t, err)
}

func (f *chiefFleet) thread(t *testing.T, s *Service, i int) trace.Thread {
	t.Helper()
	th, err := f.repo(t, s, i).ChiefOfStaffThread(f.streams[i])
	fixturekit.Must(t, err)
	return th
}

func (f *chiefFleet) turn(t *testing.T, s *Service, i int, turn string) (trace.Thread, trace.QueuedTurn) {
	t.Helper()
	th := f.thread(t, s, i)
	for _, q := range th.Turns {
		if q.Request.TurnID == turn {
			return th, q
		}
	}
	t.Fatalf("turn %s not queued on project %d", turn, i)
	return th, trace.QueuedTurn{}
}

func (f *chiefFleet) reason(t *testing.T, s *Service, i int, turn string) string {
	t.Helper()
	th, q := f.turn(t, s, i, turn)
	return s.queuedReason(th, q)
}

func awaitChiefSlots(t *testing.T, s *Service, used int, waiting ...string) ChiefSlotStatus {
	t.Helper()
	intakefixture.Soon(t, fmt.Sprintf("%d chief slots held and waiting %v", used, waiting), func() bool {
		st := s.ChiefSlots()
		return st.Used == used && slices.Equal(chiefTurns(st.Waiting), waiting)
	})
	return s.ChiefSlots()
}

func chiefTurns(turns []ChiefSlotTurn) []string {
	out := []string{}
	for _, t := range turns {
		out = append(out, t.Turn)
	}
	return out
}

func awaitStarted(t *testing.T, a *chiefAgent, turns ...string) {
	t.Helper()
	intakefixture.Soon(t, fmt.Sprintf("turns %v to start", turns), func() bool {
		for _, turn := range turns {
			if !a.hasStarted(turn) {
				return false
			}
		}
		return true
	})
}

func (f *chiefFleet) settled(t *testing.T, s *Service, i int, turn string) bool {
	_, q := f.turn(t, s, i, turn)
	return !q.CompletedAt.IsZero()
}

func (f *chiefFleet) awaitSettled(t *testing.T, s *Service, i int, turns ...string) {
	t.Helper()
	intakefixture.Soon(t, fmt.Sprintf("turns %v to settle on project %d", turns, i), func() bool {
		for _, turn := range turns {
			if !f.settled(t, s, i, turn) {
				return false
			}
		}
		return true
	})
}
