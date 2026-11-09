package intake

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// webFixture is the dependencies of the architect and committee turn paths
// over one trace repository.
type webFixture struct {
	cfg     *config.Config
	repo    *trace.Repository
	drafter *Drafter
	debate  *Debate
}

func newWebFixture(t *testing.T) *webFixture {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.Load(fixturekit.ConfigAt(t, t.TempDir()))
	fixturekit.Must(t, err)
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), owner)
	fixturekit.Must(t, err)
	t.Cleanup(func() { fixturekit.Must(t, repo.Close()) })
	fixturekit.Must(t, repo.CreateWorkstream(ctx, fixturekit.Stream, clock.Now(), owner))
	fixturekit.Must(t, os.MkdirAll(filepath.Join(cfg.Root.String(), "views"), 0700))
	f := &webFixture{cfg: cfg, repo: repo}
	profile := coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}
	about := func(*trace.Repository) *config.Config { return cfg }
	memory := func(turns *isolation.Turns, _ *config.Config, _ *trace.Repository) *isolation.Turns { return turns }
	roleExecution := func(*config.Config, string) (coreadapter.Profile, coreadapter.ExecutionSettings, error) {
		return profile, coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}, nil
	}
	f.drafter = &Drafter{Repository: repo, S: &DrafterDependencies{Now: clock.Now, About: about, Current: func() *config.Config { return cfg }, MemoryTurns: memory, RoleExecution: roleExecution}}
	f.debate = &Debate{Repository: repo, S: &DebateDependencies{Now: clock.Now, About: about, Current: func() *config.Config { return cfg }, MemoryTurns: memory, RoleExecution: roleExecution}}
	return f
}

// run runs one turn of role through turns, which the caller points at a fresh
// recording engine and host, and whose selection and service tools are stubbed: the grants, the role ceiling, the system prompt
// and the engine path are the production ones.
func (f *webFixture) run(t *testing.T, turns *isolation.Turns, engine *adaptertest.Engine, hosts *adaptertest.RecordingHosts, role, system, backend string) adaptertest.Turn {
	t.Helper()
	workspace := t.TempDir()
	fixturekit.Must(t, os.WriteFile(filepath.Join(workspace, "spec.md"), []byte("spec\n"), 0600))
	turns.Select = func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
		return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: []string{"spec.md"}, Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
	}
	granted := slices.DeleteFunc(slices.Clone(turns.Grants[role].Tools), func(name string) bool { return name == "file_read" })
	turns.Scoped = func(context.Context, coreadapter.Scope) ([]coreadapter.Tool, error) {
		return adaptertest.StubTools(granted...), nil
	}
	_, err := turns.Run(context.Background(), coreadapter.PreparedTurn{Scope: coreadapter.Scope{Project: string(f.repo.Project()), Workstream: string(fixturekit.Stream), Role: role, Thread: role, Turn: "turn-" + backend}, Profile: coreadapter.Profile{Name: role, Backend: backend, Model: "test"}, SessionDirectory: t.TempDir(), SystemPrompt: system, Prompt: "Work."})
	fixturekit.Must(t, err)
	got, ok := adaptertest.LastTurn(engine, hosts)
	if !ok {
		t.Fatal("the turn did not reach the engine")
	}
	return got
}

// Every architect turn path, run through its real constructor and
// isolation.Turns into a fake engine, is granted the backend's web tools and
// told about them, with the service tools it had before: drafting, the shed
// reply and the redraft it can deliver, the amendment debate's reply and
// amendment drafting.
func TestArchitectTurnPathsHoldWebTools(t *testing.T) {
	t.Parallel()
	f := newWebFixture(t)
	project := f.cfg.Project
	drafting := []string{"file_read", FactoryContextTool, ReadRemoteFileTool, "inspect_code", DraftTool, questions.AskTool}
	// The shed reply path also delivers the redraft, which is why it names the
	// draft delivery tool beside reply.
	reply := []string{"file_read", shed.ReplyTool, DraftTool, questions.AskTool}
	// The amendment drafter embeds the drafter, so its turns are the drafting
	// path's: it promotes Turns rather than defining a path of its own.
	amending := &AmendmentDrafter{f.drafter}
	amendment := &AmendmentDebate{f.debate}
	// Amendment drafting builds its own system prompt: read the one it queues.
	fixturekit.Must(t, f.drafter.EnsureThread(context.Background(), fixturekit.Stream))
	fixturekit.Must(t, amending.Enqueue(context.Background(), f.cfg, fixturekit.Stream, "a1", 1, "operation", nil))
	thread, err := f.repo.Thread(fixturekit.Stream, ArchitectAgent)
	fixturekit.Must(t, err)
	amendPrompt := thread.Turns[len(thread.Turns)-1].Request.SystemPrompt
	if !strings.Contains(amendPrompt, "Draft a revision to the sealed spec and plan") {
		t.Fatalf("amendment drafting prompt %q", amendPrompt)
	}
	for name, path := range map[string]struct {
		turns  func() *isolation.Turns
		prompt string
		mcp    []string
	}{
		"drafting":               {func() *isolation.Turns { return f.drafter.Turns(fixturekit.Stream) }, ArchitectSystemPrompt(project), drafting},
		"shed reply and redraft": {func() *isolation.Turns { return f.debate.ReplyPath(fixturekit.Stream, RoundInput{Round: 1}) }, ArchitectSystemPrompt(project), reply},
		"amendment drafting":     {func() *isolation.Turns { return amending.Turns(fixturekit.Stream) }, amendPrompt, drafting},
		"amendment reply":        {func() *isolation.Turns { return amendment.ReplyTurns(fixturekit.Stream, "a1", 1, nil) }, ArchitectSystemPrompt(project), []string{"file_read", shed.ReplyTool}},
	} {
		for backend := range adaptertest.WebTools {
			t.Run(name+"/"+backend, func(t *testing.T) {
				engine, hosts := &adaptertest.Engine{}, &adaptertest.RecordingHosts{}
				turns := path.turns()
				turns.Engine, turns.Hosts = engine, hosts
				got := f.run(t, turns, engine, hosts, ArchitectRole, path.prompt, backend)
				adaptertest.CheckWebTurn(t, got, backend, path.mcp, path.prompt)
			})
		}
	}
}

// A committee member's plan-debate turn and amendment-debate turn, run through
// the debate's real constructor and isolation.Turns into a fake engine, hold
// the backend's web tools and are told about them, with the service tools
// they had before.
func TestCommitteeDebateTurnsHoldWebTools(t *testing.T) {
	t.Parallel()
	f := newWebFixture(t)
	project := f.cfg.Project
	mcp := []string{"file_read", shed.ObjectTool, shed.ConcedeTool, questions.AskTool}
	for name, round := range map[string]RoundInput{
		"plan debate":      {Round: 1},
		"amendment debate": {Round: 1, Amendment: "a1"},
	} {
		prompt := AmendmentCommitteeSystemPrompt(project, round)
		if round.Amendment != "" && prompt == CommitteeSystemPrompt(project) {
			t.Fatal("the amendment debate has no prompt of its own")
		}
		for backend := range adaptertest.WebTools {
			t.Run(name+"/"+backend, func(t *testing.T) {
				engine, hosts := &adaptertest.Engine{}, &adaptertest.RecordingHosts{}
				turns := f.debate.Turns(fixturekit.Stream, round)
				turns.Engine, turns.Hosts = engine, hosts
				got := f.run(t, turns, engine, hosts, CommitteeRole, prompt, backend)
				adaptertest.CheckWebTurn(t, got, backend, mcp, prompt)
			})
		}
	}
}
