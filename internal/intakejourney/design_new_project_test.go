package intakejourney

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A design session opened for a new project is hosted in an existing project
// and its brief may describe the project to create. draft_brief records a
// valid new_project alone or beside existing projects, refuses an invalid one
// and a brief with neither, and the design view returns the latest new_project.
func TestDraftBriefRecordsAndValidatesANewProject(t *testing.T) {
	t.Parallel()
	_, c, home, engine := newDesignEnforcementFixture(t)
	ctx := context.Background()
	other := addDesignProject(t, c, "client")
	inactive := addDesignProject(t, c, "inactive")
	_, err := c.RemoveProject(ctx, inactive)
	must(t, err)

	opened, err := c.OpenNewProjectDesign(ctx, "")
	must(t, err)
	stream := opened.Design.Workstream
	if opened.Design.Project != home || len(opened.Design.Projects) != 0 || opened.Design.NewProject != nil {
		t.Fatalf("new-project session opened in the first active project with no default list: %+v", opened.Design)
	}

	project := func(extra map[string]any) map[string]any {
		np := map[string]any{"name": "Gadget", "repo": "gadget", "charter": "1. Keep it small.\n"}
		for k, v := range extra {
			np[k] = v
		}
		return np
	}
	var turnMu sync.Mutex
	var turnErr error
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (result *agent.Result, err error) {
		defer func() {
			if err != nil {
				turnMu.Lock()
				if turnErr == nil {
					turnErr = err
				}
				turnMu.Unlock()
			}
		}()
		for _, want := range []string{"initialised with only a README", "`dagger check`", "Visibility stays private unless the owner confirms public", "You create nothing and hold no credential"} {
			if !strings.Contains(req.SystemPrompt, want) {
				return nil, fmt.Errorf("design prompt lacks %q", want)
			}
		}
		if !strings.HasPrefix(req.SystemPrompt, designRolePrompt) {
			return nil, fmt.Errorf("design prompt does not start with the role prompt")
		}
		names, err := toolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if want := []string{"draft_brief", "file_read", "read_repo"}; !slices.Equal(names, want) {
			return nil, fmt.Errorf("design tools %v, want %v", names, want)
		}
		call := func(input map[string]any) (string, error) {
			input["content"] = productionBrief
			return callTool(ctx, tools, draftBriefToolName, input)
		}
		refused := func(label string, input map[string]any) error {
			out, err := call(input)
			if err != nil || !strings.Contains(out, `"recorded":false`) {
				return fmt.Errorf("%s was not refused: %s %v", label, out, err)
			}
			return nil
		}
		recorded := func(label string, input map[string]any) error {
			out, err := call(input)
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				return fmt.Errorf("%s was not recorded: %s %v", label, out, err)
			}
			return nil
		}
		for label, input := range map[string]map[string]any{
			"no project and no new project": {"projects": []config.ProjectID{}},
			"invalid repo name":             {"new_project": project(map[string]any{"repo": "not/a repo"})},
			"repo name ending in .git":      {"new_project": project(map[string]any{"repo": "gadget.git"})},
			"empty display name":            {"new_project": project(map[string]any{"name": "  "})},
			"empty charter":                 {"new_project": project(map[string]any{"charter": ""})},
			"unknown visibility":            {"new_project": project(map[string]any{"visibility": "internal"})},
			"inactive project":              {"projects": []config.ProjectID{inactive}, "new_project": project(nil)},
			"unknown project":               {"projects": []string{"p_0123456789abcdef0123456789abcdef"}, "new_project": project(nil)},
		} {
			if err := refused(label, input); err != nil {
				return nil, err
			}
		}
		if err := recorded("a new project alone", map[string]any{"new_project": project(map[string]any{"description": "A gadget."})}); err != nil {
			return nil, err
		}
		if err := recorded("a new project beside an existing project", map[string]any{"projects": []config.ProjectID{other}, "new_project": project(map[string]any{"visibility": "public"})}); err != nil {
			return nil, err
		}
		if err := recorded("a brief that omits new_project", map[string]any{}); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "design-new-project", ResultText: "new project recorded"}, nil
	}
	engine.Mu.Unlock()
	_, err = c.DesignMessage(ctx, stream, "Design a gadget project.")
	must(t, err)
	conversation := awaitDesignConversation(t, c, stream, func(r ConversationResponse) bool {
		return len(r.Entries) > 0 && (r.Entries[len(r.Entries)-1].State == api.TurnFailed || r.Entries[len(r.Entries)-1].Kind == "response")
	})
	if last := conversation.Entries[len(conversation.Entries)-1]; last.State == api.TurnFailed || last.Kind != "response" {
		turnMu.Lock()
		cause := turnErr
		turnMu.Unlock()
		t.Fatalf("design turn failed: %v\n%+v", cause, conversation)
	}

	view, err := c.Design(ctx, stream)
	must(t, err)
	got := view.Design
	// Three recorded briefs: the last omitted new_project and kept the second's.
	if got.BriefRevision != 3 || !slices.Equal(got.Projects, []config.ProjectID{other}) || got.NewProject == nil {
		t.Fatalf("latest brief: %+v", got)
	}
	if *got.NewProject != (api.NewProject{Name: "Gadget", Repo: "gadget", Visibility: api.VisibilityPublic, Charter: "1. Keep it small."}) {
		t.Fatalf("new project: %+v", got.NewProject)
	}
}

// Visibility is private when the agent leaves it out, the description stays
// optional, and an explicit null removes the new project again.
func TestDraftBriefNewProjectDefaultsToPrivateAndNullRemovesIt(t *testing.T) {
	t.Parallel()
	_, c, _, engine := newDesignEnforcementFixture(t)
	ctx := context.Background()
	opened, err := c.OpenNewProjectDesign(ctx, "")
	must(t, err)
	stream := opened.Design.Workstream
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		out, err := callTool(ctx, tools, draftBriefToolName, map[string]any{"content": productionBrief,
			"new_project": map[string]any{"name": " Gadget ", "repo": "gadget", "charter": "Rules."}})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("new project: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "design-new-project", ResultText: "ok"}, nil
	}
	engine.Mu.Unlock()
	_, err = c.DesignMessage(ctx, stream, "Draft it.")
	must(t, err)
	awaitDesignConversation(t, c, stream, func(r ConversationResponse) bool {
		return len(r.Entries) > 1 && r.Entries[len(r.Entries)-1].Kind == "response"
	})
	view, err := c.Design(ctx, stream)
	must(t, err)
	if np := view.Design.NewProject; np == nil || np.Name != "Gadget" || np.Visibility != api.VisibilityPrivate || np.Description != "" {
		t.Fatalf("new project %+v, want private", np)
	}
	if len(view.Design.Projects) != 0 {
		t.Fatalf("the host project joined the new-project brief: %v", view.Design.Projects)
	}
	listed, err := c.Designs(ctx, "")
	must(t, err)
	if len(listed.Designs) != 1 || listed.Designs[0].NewProject == nil || listed.Designs[0].NewProject.Visibility != api.VisibilityPrivate {
		t.Fatalf("design list: %+v", listed.Designs)
	}
}

// A new-project session is hosted in the project named in the request, or the
// first active project, and is refused when no project is active.
func TestOpenNewProjectDesignChoosesItsHost(t *testing.T) {
	t.Parallel()
	_, c, home, _ := newDesignEnforcementFixture(t)
	ctx := context.Background()
	other := addDesignProject(t, c, "client")
	for _, tc := range []struct{ asked, host config.ProjectID }{{"", home}, {other, other}, {home, home}} {
		opened, err := c.OpenNewProjectDesign(ctx, tc.asked)
		must(t, err)
		if opened.Design.Project != tc.host {
			t.Fatalf("asked %q: hosted in %s, want %s", tc.asked, opened.Design.Project, tc.host)
		}
	}
	// An ordinary session still defaults to its host project.
	ordinary, err := c.OpenDesign(ctx, other)
	must(t, err)
	if len(ordinary.Design.Projects) != 1 || ordinary.Design.Projects[0] != other {
		t.Fatalf("ordinary design: %+v", ordinary.Design)
	}

	empty, _ := projectFixture(t)
	_, none := start(t, empty)
	_, err = none.OpenNewProjectDesign(ctx, "")
	apiError(t, err, NoProject, "a new-project design session lives in an existing project, and no project is active; add one with osmia project add first")
	designs, err := none.Designs(ctx, "")
	must(t, err)
	if len(designs.Designs) != 0 {
		t.Fatalf("a session opened without an active project: %+v", designs.Designs)
	}
}
