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
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// WiringSetup supplies two projects and their shared chief's fake execution backend.
type WiringSetup struct {
	Config  *config.Config
	Clock   *fixturekit.FixedClock
	Engine  *fixturekit.DemoEngine
	Ticks   chan time.Time
	Threads func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error)
}

func NewWiringSetup(t *testing.T, home string, cfgOptions config.Options, agents map[string]string, observe func(*trace.Repository)) *WiringSetup {
	t.Helper()
	ctx := context.Background()
	root := cfgOptions.Root
	home2 := filepath.Join(home, "repo2")
	fixturekit.RegisterProject(t, root, fixturekit.OtherProject, `version = 1
upstream = "upstream/other"
fork = "owner/other"
clone = "`+home2+`"
`)
	cfg, err := projectstore.LoadConfig(cfgOptions)
	Must(t, err)

	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	f := &WiringSetup{Config: cfg, Clock: clock, Ticks: make(chan time.Time)}
	for _, p := range cfg.Projects {
		Must(t, os.MkdirAll(p.Clone, 0700))
		repo, err := trace.Create(ctx, cfg.Root, p, clock.Now(), owner)
		Must(t, err)
		ws, agentID, threadID := fixturekit.Stream, DemoAgent, DemoThread
		if p.ID == fixturekit.OtherProject {
			ws, agentID, threadID = fixturekit.OtherStream, "agent_mason2", "thread_mason2"
		}
		Must(t, repo.CreateWorkstream(ctx, ws, clock.Now(), owner))
		Must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: agentID, Project: p.ID, Workstream: ws, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: "mason", ThreadID: threadID}))
		// The chief's answer to member1's question cites charter#1, which
		// must resolve against member1's own project (through ProjectFor),
		// as in newQuestionFixture.
		fixturekit.RecordCharter(t, repo, "# Charter\n\n1. Keep state in files under the root.\n")
		Must(t, repo.Close())
	}

	sessions := &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	f.Engine = &fixturekit.DemoEngine{Sessions: sessions, Turns: map[string]DemoTurn{}}
	f.Engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	views := filepath.Join(cfg.Root.String(), "views")
	Must(t, os.Mkdir(views, 0700))
	every := append([]string{"file_read", questions.AskTool}, questions.ChiefTools...)
	f.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		observe(r)
		turns := &isolation.Turns{Workspaces: &fixturekit.DemoWorkspaces{}, Views: isolation.Views{Directory: views}, Engine: f.Engine,
			Grants: map[string]coreadapter.Capabilities{"mason": {Tools: every}, "reviewer": {Tools: every}, trace.ChiefOfStaff: {Tools: every}},
			Select: func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
				return isolation.Selection{Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
			},
			Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				list, err := questions.Tools(r, agents[scope.Thread], scope, clock.Now)
				if scope.Role == trace.ChiefOfStaff {
					list = jevfixture.AutoDecide(r, config.WorkstreamID(scope.Workstream), list)
				}
				return list, err
			},
			Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}},
		}
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: &questions.Turns{Turns: turns, Repository: r}, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				directory := filepath.Join(cfg.Root.String(), "sessions", string(r.Project()), in.Agent, in.Turn)
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			}}, nil
	}
	return f
}
