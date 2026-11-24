package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

var webToolsByBackend = map[string][]string{
	"claude":   {"WebSearch", "WebFetch"},
	"codex":    {"web_search"},
	"opencode": {"websearch", "webfetch"},
}

var writeOrShellTools = []string{"Write", "Edit", "Bash", "apply_patch", "shell", "edit", "bash"}

func enforcedTurns(t *testing.T, f *shedFixture, repository *trace.Repository) (*isolation.Turns, *adaptertest.Engine, *adaptertest.RecordingHosts) {
	t.Helper()
	bound, err := Enforce(f.s.options, Enforcement{}).Threads(repository, f.s.cfg)
	intakefixture.Must(t, err)
	turns := bound.(thread.Dispatcher).Runner.Turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
	engine, hosts := &adaptertest.Engine{}, &adaptertest.RecordingHosts{}
	turns.Engine, turns.Hosts = engine, hosts
	return turns, engine, hosts
}

func runEnforced(t *testing.T, turns *isolation.Turns, engine *adaptertest.Engine, hosts *adaptertest.RecordingHosts, scope coreadapter.Scope, backend string) adaptertest.Turn {
	t.Helper()
	_, err := turns.Run(context.Background(), coreadapter.PreparedTurn{Scope: scope, Profile: coreadapter.Profile{Name: scope.Role, Backend: backend, Model: "test"}, SessionDirectory: t.TempDir(),
		SystemPrompt: "You are the " + scope.Role + ".", Prompt: "Work."})
	intakefixture.Must(t, err)
	got, ok := adaptertest.LastTurn(engine, hosts)
	if !ok {
		t.Fatal("the turn did not reach the engine")
	}
	return got
}

func TestOrdinaryChiefTurnHoldsWebTools(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "chief-web")
	for backend, web := range webToolsByBackend {
		t.Run(backend, func(t *testing.T) {
			turns, engine, hosts := enforcedTurns(t, f, repository)
			got := runEnforced(t, turns, engine, hosts, coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "turn-" + backend}, backend)
			for _, tool := range web {
				if !slices.Contains(got.Native, tool) {
					t.Errorf("%s chief native tools %v lack %s", backend, got.Native, tool)
				}
			}
			if backend == "claude" {
				if want := []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch", "mcp__osmia_0"}; !slices.Equal(got.Native, want) {
					t.Errorf("native tools %v, want %v", got.Native, want)
				}
			}
			for _, tool := range writeOrShellTools {
				if slices.Contains(got.Native, tool) {
					t.Errorf("chief holds %s", tool)
				}
			}
			names := slices.Clone(got.MCP)
			want := slices.Clone(chiefOrdinaryTools)
			slices.Sort(names)
			slices.Sort(want)
			if !slices.Equal(names, want) {
				t.Errorf("chief MCP tools %v, want %v", names, want)
			}
			for _, name := range names {
				if lower := strings.ToLower(name); strings.Contains(lower, "web") || strings.Contains(lower, "search") || strings.Contains(lower, "fetch") {
					t.Errorf("service tool %s looks like a web tool", name)
				}
			}
			if slices.Contains(names, draftBriefToolName) || slices.Contains(names, designReadRepoToolName) || slices.Contains(names, designTitleToolName) {
				t.Errorf("an ordinary chief reaches a design tool: %v", names)
			}
			if !got.Capabilities.Web || got.Capabilities.WriteFiles || got.Capabilities.Execute || got.Capabilities.Network {
				t.Errorf("host capabilities %+v", got.Capabilities)
			}
			if len(got.Mounts) == 0 || got.Mounts[0].Access != agent.ReadOnly || got.VCS {
				t.Errorf("chief workspace %+v vcs=%v", got.Mounts, got.VCS)
			}
			if !strings.HasPrefix(got.SystemPrompt, "You are the chief_of_staff.") || !strings.Contains(got.SystemPrompt, isolation.WebGuidance) {
				t.Errorf("chief system prompt lacks the web guidance: %q", got.SystemPrompt)
			}
		})
	}
}

func TestWebIsGrantedToDesignAndSessionRoles(t *testing.T) {
	t.Parallel()
	f, _, repository := newReviewFixture(t, "design-web")
	turns, _, _ := enforcedTurns(t, f, repository)
	if !turns.Grants[trace.ChiefOfStaff].Web || !chiefOrdinaryNarrow.Web || !designNarrow.Web {
		t.Fatalf("chief grant %+v, ordinary narrowing %+v, design narrowing %+v", turns.Grants[trace.ChiefOfStaff], chiefOrdinaryNarrow, designNarrow)
	}
	if want := []string{"file_read", draftBriefToolName, designReadRepoToolName, designTitleToolName}; !slices.Equal(designNarrow.Tools, want) {
		t.Fatalf("design narrowing tools %v, want %v", designNarrow.Tools, want)
	}
	if grant := turns.Grants[sessionRole]; !grant.Web || !grant.WriteFiles || !grant.Execute || grant.Network {
		t.Fatalf("session grant %+v", grant)
	}
}

func TestRolesWithoutWebGetNoWebTools(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "no-web")
	scope := func(role, thread string) coreadapter.Scope {
		return coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: role, Thread: thread, Unit: "unit", Turn: "turn"}
	}
	for name, s := range map[string]coreadapter.Scope{
		"unit mason":     scope(masonRole, masonAgent("unit")),
		"drift mason":    scope(masonRole, driftMasonAgent),
		"unit reviewer":  scope(reviewerRole, reviewerAgent("unit")),
		"drift reviewer": scope(reviewerRole, driftReviewerAgent),
	} {
		for backend, web := range webToolsByBackend {
			t.Run(name+"/"+backend, func(t *testing.T) {
				turns, engine, hosts := enforcedTurns(t, f, repository)
				workspace := t.TempDir()
				intakefixture.Must(t, os.WriteFile(filepath.Join(workspace, "README.md"), []byte("hi\n"), 0600))
				turns.Workspaces = stagedWorkspaces{}
				turns.Capture, turns.ViewTools, turns.Context = nil, nil, nil
				turns.Select = func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
					return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: []string{"README.md"}, Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
				}
				granted := slices.DeleteFunc(slices.Clone(turns.Grants[s.Role].Tools), func(tool string) bool { return tool == "file_read" || tool == "file_write" })
				turns.Scoped = func(context.Context, coreadapter.Scope) ([]coreadapter.Tool, error) {
					return adaptertest.StubTools(granted...), nil
				}
				got := runEnforced(t, turns, engine, hosts, s, backend)
				for _, tool := range web {
					if slices.Contains(got.Native, tool) {
						t.Errorf("%s holds %s on %s: %v", name, tool, backend, got.Native)
					}
				}
				if got.Capabilities.Web {
					t.Errorf("host told %s holds Web: %+v", name, got.Capabilities)
				}
				if strings.Contains(got.SystemPrompt, isolation.WebGuidance) || !strings.HasPrefix(got.SystemPrompt, "You are the "+s.Role+".") {
					t.Errorf("%s system prompt %q", name, got.SystemPrompt)
				}
			})
		}
	}
	turns, _, _ := enforcedTurns(t, f, repository)
	for role, grant := range turns.Grants {
		if grant.Web != (role == trace.ChiefOfStaff || role == sessionRole) {
			t.Errorf("%s grant %+v", role, grant)
		}
	}
}
