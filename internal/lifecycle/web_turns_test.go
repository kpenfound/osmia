package lifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// webTurnController is a lifecycle controller wired to recording fake
// engines: one for the architect and one for the committee.
func webTurnController(t *testing.T) (*lifecycle.Controller, *adaptertest.Engine, *adaptertest.RecordingHosts, *adaptertest.Engine, *adaptertest.RecordingHosts) {
	t.Helper()
	cfg, err := config.Load(fixturekit.ConfigAt(t, t.TempDir()))
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.MkdirAll(filepath.Join(cfg.Root.String(), "views"), 0700))
	architect, architectHosts := &adaptertest.Engine{}, &adaptertest.RecordingHosts{}
	committee, committeeHosts := &adaptertest.Engine{}, &adaptertest.RecordingHosts{}
	c := lifecycle.New(lifecycle.Dependencies{
		Current:     func() *config.Config { return cfg },
		About:       func(*trace.Repository) *config.Config { return cfg },
		MemoryTurns: func(turns *isolation.Turns, _ *config.Config, _ *trace.Repository) *isolation.Turns { return turns },
		RoleExecution: func(*config.Config, string) (coreadapter.Profile, coreadapter.ExecutionSettings, error) {
			return coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}, nil
		},
		Options: lifecycle.Execution{
			Architect: &intake.Architect{Engine: architect, Hosts: architectHosts},
			Committee: &intake.Committee{Engine: committee, Hosts: committeeHosts},
		},
	})
	return c, architect, architectHosts, committee, committeeHosts
}

// runWebTurn runs one turn of role through turns on backend and reads what
// reached the engine and the host the controller wired it to. The turns keep
// their production grants, role ceiling, system prompt and engine; the view is
// a stub, as selecting the real one needs a sealed plan.
func runWebTurn(t *testing.T, turns *isolation.Turns, engine *adaptertest.Engine, hosts *adaptertest.RecordingHosts, role string, tools []string, system, backend string) adaptertest.Turn {
	t.Helper()
	workspace := t.TempDir()
	fixturekit.Must(t, os.WriteFile(filepath.Join(workspace, "spec.md"), []byte("spec\n"), 0600))
	turns.Select = func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
		return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: []string{"spec.md"}, Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
	}
	turns.Scoped = func(context.Context, coreadapter.Scope) ([]coreadapter.Tool, error) {
		return adaptertest.StubTools(tools...), nil
	}
	_, err := turns.Run(context.Background(), coreadapter.PreparedTurn{Scope: coreadapter.Scope{Project: "p", Workstream: string(fixturekit.Stream), Role: role, Thread: role, Turn: "turn-" + backend}, Profile: coreadapter.Profile{Name: role, Backend: backend, Model: "test"}, SessionDirectory: t.TempDir(), SystemPrompt: system, Prompt: "Work."})
	fixturekit.Must(t, err)
	got, ok := adaptertest.LastTurn(engine, hosts)
	if !ok {
		t.Fatal("the turn did not reach the engine")
	}
	return got
}

// The workgroup turn paths the architect and the committee run, built by
// their real constructors and run through isolation.Turns into a fake engine,
// hold the backend's web tools and are told about them on every backend: the
// architect's project-spec draft, and the delta debate's deliver_delta,
// answer_delta and review_delta. The service tool each turn kept is the only
// one it serves, and its view is read-only with no write, shell or Network.
func TestWorkgroupTurnPathsHoldWebTools(t *testing.T) {
	t.Parallel()
	c, architect, architectHosts, committee, committeeHosts := webTurnController(t)
	change := lifecycle.WorkgroupChange{ID: "c1", Workgroup: "g1", Host: fixturekit.Stream}
	const architectRole, committeeRole = intake.ArchitectRole, intake.CommitteeRole
	type path struct {
		role, prompt string
		// tools are the service tools the turn serves; read is whether it
		// also holds the file reading tool.
		tools  []string
		read   bool
		turns  func() *isolation.Turns
		engine *adaptertest.Engine
		hosts  *adaptertest.RecordingHosts
	}
	delta := func(role, agent, tool string) func() *isolation.Turns {
		return func() *isolation.Turns {
			turns, err := c.ChangeTurns(context.Background(), nil, change, role, agent, tool, role, t.TempDir())
			fixturekit.Must(t, err)
			return turns
		}
	}
	paths := map[string]path{
		"project spec draft": {role: architectRole, prompt: lifecycle.ProjectSpecDraftSystemPrompt(), tools: []string{lifecycle.ProjectSpecDraftTool}, turns: func() *isolation.Turns {
			return c.ProjectSpecDraftTurns(nil, trace.Workgroup{}, fixturekit.Stream, nil, lifecycle.ProjectSpecDraftInput{Turn: "t1"})
		}, engine: architect, hosts: architectHosts},
		"deliver delta": {role: architectRole, prompt: lifecycle.DeltaSystemPrompt(architectRole), tools: []string{lifecycle.DeltaDraftTool}, read: true, turns: delta(architectRole, "architect", lifecycle.DeltaDraftTool), engine: architect, hosts: architectHosts},
		"answer delta":  {role: architectRole, prompt: lifecycle.DeltaSystemPrompt(architectRole), tools: []string{lifecycle.DeltaReplyTool}, read: true, turns: delta(architectRole, "architect", lifecycle.DeltaReplyTool), engine: architect, hosts: architectHosts},
		"review delta":  {role: committeeRole, prompt: lifecycle.DeltaSystemPrompt(committeeRole), tools: []string{lifecycle.DeltaReviewTool}, read: true, turns: delta(committeeRole, "member", lifecycle.DeltaReviewTool), engine: committee, hosts: committeeHosts},
	}
	for name, p := range paths {
		for backend := range adaptertest.WebTools {
			t.Run(name+"/"+backend, func(t *testing.T) {
				turns := p.turns()
				got := runWebTurn(t, turns, p.engine, p.hosts, p.role, p.tools, p.prompt, backend)
				want := slices.Clone(p.tools)
				if p.read {
					// The file reading tool is served beside the turn's own.
					want = append(want, "file_read")
				}
				adaptertest.CheckWebTurn(t, got, backend, want, p.prompt)
				if grant := turns.Grants[p.role]; !grant.Web || grant.Network || grant.WriteFiles || grant.Execute {
					t.Errorf("grant %+v", grant)
				}
			})
		}
	}
}
