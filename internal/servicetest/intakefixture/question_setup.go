package intakefixture

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"

	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type QuestionAsker struct {
	Stream                    config.WorkstreamID
	Agent, Thread, Role, Turn string
}
type QuestionSetup struct {
	Options config.Options
	Config  *config.Config
	Clock   *fixturekit.FixedClock
	Engine  *fixturekit.DemoEngine
	Lives   chan *trace.Repository
	Ticks   chan time.Time
	Threads func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error)
}

// NewQuestionSetup seeds askers and fake execution without depending on a service.
func NewQuestionSetup(t *testing.T, prefix, global, charter string, askers []QuestionAsker, fixture func(*testing.T, string) config.Options) *QuestionSetup {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", prefix)
	Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	f := &QuestionSetup{Options: fixture(t, home), Clock: &fixturekit.FixedClock{At: fixturekit.DemoStart}, Lives: make(chan *trace.Repository, 1), Ticks: make(chan time.Time)}
	path := filepath.Join(f.Options.Root, "config.toml")
	data, err := os.ReadFile(path)
	Must(t, err)
	Must(t, os.WriteFile(path, append(data, []byte(global)...), 0600))
	f.Config, err = config.Load(f.Options)
	Must(t, err)
	cfg, clock := f.Config, f.Clock
	Must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	fixturekit.DemoGit(t, home, "-C", cfg.Project.Clone, "init", "-q")

	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	Must(t, err)
	fixturekit.RecordCharter(t, repo, charter)
	agents := map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff}
	created := map[config.WorkstreamID]bool{}
	for _, a := range askers {
		if !created[a.Stream] {
			Must(t, repo.CreateWorkstream(ctx, a.Stream, clock.Now(), owner))
			created[a.Stream] = true
		}
		agents[a.Thread] = a.Agent
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: a.Agent, Project: fixturekit.Project, Workstream: a.Stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}
		Must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: a.Role, ThreadID: a.Thread}))
		h.Schema, h.ID, h.Cause, h.Depth = "osmia.trace.turn-request", "request_"+a.Turn, "message_"+a.Turn, 1
		_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: a.Agent, ThreadID: a.Thread, TurnID: a.Turn,
			Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the " + a.Role + ".", Prompt: "Work: " + a.Turn})
		Must(t, err)
	}
	Must(t, repo.Close())

	sessions := &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	f.Engine = &fixturekit.DemoEngine{Sessions: sessions, Turns: map[string]fixturekit.DemoTurn{}}
	f.Engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	views := filepath.Join(cfg.Root.String(), "views")
	Must(t, os.Mkdir(views, 0700))

	every := append([]string{"file_read", questions.AskTool}, questions.ChiefTools...)
	f.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		f.Lives <- r
		turns := &isolation.Turns{Workspaces: &fixturekit.DemoWorkspaces{Directory: cfg.Project.Clone}, Views: isolation.Views{Directory: views}, Engine: f.Engine,
			Grants: map[string]coreadapter.Capabilities{"mason": {Tools: every}, "reviewer": {Tools: every}, trace.ChiefOfStaff: {Tools: every}},
			Select: func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
				return isolation.Selection{Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
			},
			Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				return questions.Tools(r, agents[scope.Thread], scope, clock.Now)
			},
			Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}},
		}
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: &questions.Turns{Turns: turns, Repository: r}, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				directory := filepath.Join(cfg.Root.String(), "sessions", in.Agent, in.Turn)
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			}}, nil
	}
	return f
}
func SettleTicks(t *testing.T, ticks chan<- time.Time, now func() time.Time, closeService func() error) {
	t.Helper()
	for range 5 {
		select {
		case ticks <- now():
		case <-time.After(DemoTimeout):
			closeService()
			t.Fatal("service did not finish its pass")
		}
	}
}
