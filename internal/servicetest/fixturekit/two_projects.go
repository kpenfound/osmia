package fixturekit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/trace"
)

const OtherProject config.ProjectID = "p_fedcba9876543210fedcba9876543210"
const OtherStream config.WorkstreamID = "w_fedcba9876543210fedcba9876543210"

type TwoProjectData struct {
	Config  config.Options
	Loaded  *config.Config
	Clock   *DemoClock
	Streams map[config.ProjectID]config.WorkstreamID
}

func TwoProjects(t *testing.T) TwoProjectData {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "mp-")
	Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := ConfigAt(t, home)
	root := opts.Root
	top, err := os.ReadFile(filepath.Join(root, "config.toml"))
	Must(t, err)
	Must(t, os.WriteFile(filepath.Join(root, "config.toml"), append(top, "[capacity]\nmasons = 1\n"...), 0600))
	RegisterProject(t, root, OtherProject, `version = 1
upstream = "upstream/other"
fork = "owner/other"
clone = "`+filepath.Join(home, "other")+`"
`)
	cfg, err := projectstore.LoadConfig(opts)
	Must(t, err)
	f := TwoProjectData{Config: opts, Loaded: cfg, Clock: &DemoClock{At: DemoStart},
		Streams: map[config.ProjectID]config.WorkstreamID{Project: Stream, OtherProject: OtherStream}}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	for _, p := range cfg.Projects {
		Must(t, os.MkdirAll(p.Clone, 0700))
		repo, err := trace.Create(ctx, cfg.Root, p, f.Clock.Now(), owner)
		Must(t, err)
		ws := f.Streams[p.ID]
		Must(t, repo.CreateWorkstream(ctx, ws, f.Clock.Now(), owner))
		Must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: DemoAgent, Project: p.ID, Workstream: ws, At: f.Clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: DemoRole, ThreadID: DemoThread}))
		QueueProjectTurn(t, repo, p.ID, f.Streams[p.ID], f.Clock.Now(), "first")
		Must(t, repo.Close())
	}

	return f
}

// queue accepts an owner message to the Project's mason thread.
func QueueProjectTurn(t *testing.T, repo *trace.Repository, id config.ProjectID, Stream config.WorkstreamID, at time.Time, turn string) {
	t.Helper()
	turn = string(id[len(id)-4:]) + "-" + turn
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_" + turn, Project: id, Workstream: Stream, At: at, Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "message_" + turn, Depth: 1},
		AgentID: DemoAgent, ThreadID: DemoThread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message: " + turn}
	_, err := repo.EnqueueTurn(context.Background(), req)
	Must(t, err)
}

// Sibling is a workstream id distinct from Stream.
const Sibling config.WorkstreamID = "w_0123456789abcdef0123456789abcdee"
