package delivery

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
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// A committee member's final-review turn, run through the final reviewer's
// real turn path and isolation.Turns into a fake engine on every backend, is
// granted no web tool and told nothing about the web: web access belongs to
// the committee's plan and amendment debate alone.
func TestFinalReviewTurnHoldsNoWebTools(t *testing.T) {
	t.Parallel()
	cfg, repo := deliveryTrace(t)
	workspace := t.TempDir()
	fixturekit.Must(t, os.WriteFile(filepath.Join(workspace, "spec.md"), []byte("spec\n"), 0600))
	views := filepath.Join(cfg.Root.String(), "views")
	fixturekit.Must(t, os.MkdirAll(views, 0700))
	for backend, web := range map[string][]string{"claude": {"WebSearch", "WebFetch"}, "codex": {"web_search"}, "opencode": {"websearch", "webfetch"}} {
		t.Run(backend, func(t *testing.T) {
			engine, hosts := &adaptertest.Engine{}, &adaptertest.RecordingHosts{}
			reviewer := &FinalReviewer{Repository: repo, S: New(Dependencies{
				Committee:   &intake.Committee{Engine: engine, Hosts: hosts},
				About:       func(*trace.Repository) *config.Config { return cfg },
				Current:     func() *config.Config { return cfg },
				MemoryTurns: func(turns *isolation.Turns, _ *config.Config, _ *trace.Repository) *isolation.Turns { return turns },
			})}
			turns := reviewer.Turns(stream, FinalReviewInput{Review: 1}, FinalReport{Review: 1})
			// The selection and service tools need a reviewed branch; the grant,
			// the role ceiling and the engine path are the production ones.
			turns.Select = func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
				return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: []string{"spec.md"}, Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
			}
			granted := slices.DeleteFunc(slices.Clone(turns.Grants[committeeRole].Tools), func(name string) bool { return name == "file_read" })
			turns.Scoped = func(context.Context, coreadapter.Scope) ([]coreadapter.Tool, error) {
				return adaptertest.StubTools(granted...), nil
			}
			system := FinalSystemPrompt(cfg.Project)
			_, err := turns.Run(context.Background(), coreadapter.PreparedTurn{Scope: coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Role: committeeRole, Thread: "final", Turn: "final-1"},
				Profile: coreadapter.Profile{Name: committeeRole, Backend: backend, Model: "test"}, SessionDirectory: t.TempDir(), SystemPrompt: system, Prompt: "Read."})
			fixturekit.Must(t, err)
			got, ok := adaptertest.LastTurn(engine, hosts)
			if !ok {
				t.Fatal("the turn did not reach the engine")
			}
			for _, tool := range web {
				if slices.Contains(got.Native, tool) {
					t.Errorf("final review holds %s: %v", tool, got.Native)
				}
			}
			if got.Capabilities.Web || !strings.HasPrefix(got.SystemPrompt, system) || strings.Contains(got.SystemPrompt, isolation.WebGuidance) {
				t.Errorf("final review capabilities %+v, system prompt %q", got.Capabilities, got.SystemPrompt)
			}
		})
	}
}
