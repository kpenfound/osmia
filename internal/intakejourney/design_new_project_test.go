package intakejourney

import (
	"context"
	"fmt"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDraftBriefRecordsAndValidatesANewProject(t *testing.T) {
	t.Parallel()
	_, c, home, engine := newDesignEnforcementFixture(t)
	ctx := context.Background()
	other := addDesignProject(t, c, "client")
	inactive := addDesignProject(t, c, "inactive")
	_, err := c.RemoveProject(ctx, inactive)
	fixturekit.Must(t, err)

	opened, err := c.OpenNewProjectDesign(ctx, "")
	fixturekit.Must(t, err)
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
		if !strings.HasPrefix(req.SystemPrompt, intake.DesignRolePrompt) {
			return nil, fmt.Errorf("design prompt does not start with the role prompt")
		}
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if want := []string{"draft_brief", "file_read", "read_repo", "set_title"}; !slices.Equal(names, want) {
			return nil, fmt.Errorf("design tools %v, want %v", names, want)
		}
		call := func(input map[string]any) (string, error) {
			input["content"] = intakefixture.ProductionBrief
			return fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, input)
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
	fixturekit.Must(t, err)
	conversation := awaitDesignConversation(t, c, stream, func(r api.ConversationResponse) bool {
		return len(r.Entries) > 0 && (r.Entries[len(r.Entries)-1].State == api.TurnFailed || r.Entries[len(r.Entries)-1].Kind == "response")
	})
	if last := conversation.Entries[len(conversation.Entries)-1]; last.State == api.TurnFailed || last.Kind != "response" {
		turnMu.Lock()
		cause := turnErr
		turnMu.Unlock()
		t.Fatalf("design turn failed: %v\n%+v", cause, conversation)
	}

	view, err := c.Design(ctx, stream)
	fixturekit.Must(t, err)
	got := view.Design
	if got.BriefRevision != 3 || !slices.Equal(got.Projects, []config.ProjectID{other}) || got.NewProject == nil {
		t.Fatalf("latest brief: %+v", got)
	}
	if *got.NewProject != (api.NewProject{Name: "Gadget", Repo: "gadget", Visibility: api.VisibilityPublic, Charter: "1. Keep it small."}) {
		t.Fatalf("new project: %+v", got.NewProject)
	}
}

func TestDraftBriefNewProjectDefaultsToPrivateAndNullRemovesIt(t *testing.T) {
	t.Parallel()
	_, c, _, engine := newDesignEnforcementFixture(t)
	ctx := context.Background()
	opened, err := c.OpenNewProjectDesign(ctx, "")
	fixturekit.Must(t, err)
	stream := opened.Design.Workstream
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		out, err := fixturekit.CallTool(ctx, tools, intake.DraftBriefToolName, map[string]any{"content": intakefixture.ProductionBrief,
			"new_project": map[string]any{"name": " Gadget ", "repo": "gadget", "charter": "Rules."}})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("new project: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "design-new-project", ResultText: "ok"}, nil
	}
	engine.Mu.Unlock()
	_, err = c.DesignMessage(ctx, stream, "Draft it.")
	fixturekit.Must(t, err)
	awaitDesignConversation(t, c, stream, func(r api.ConversationResponse) bool {
		return len(r.Entries) > 1 && r.Entries[len(r.Entries)-1].Kind == "response"
	})
	view, err := c.Design(ctx, stream)
	fixturekit.Must(t, err)
	if np := view.Design.NewProject; np == nil || np.Name != "Gadget" || np.Visibility != api.VisibilityPrivate || np.Description != "" {
		t.Fatalf("new project %+v, want private", np)
	}
	if len(view.Design.Projects) != 0 {
		t.Fatalf("the host project joined the new-project brief: %v", view.Design.Projects)
	}
	listed, err := c.Designs(ctx, "")
	fixturekit.Must(t, err)
	if len(listed.Designs) != 1 || listed.Designs[0].NewProject == nil || listed.Designs[0].NewProject.Visibility != api.VisibilityPrivate {
		t.Fatalf("design list: %+v", listed.Designs)
	}
}

func TestOpenNewProjectDesignChoosesItsHost(t *testing.T) {
	t.Parallel()
	_, c, home, _ := newDesignEnforcementFixture(t)
	ctx := context.Background()
	other := addDesignProject(t, c, "client")
	for _, tc := range []struct{ asked, host config.ProjectID }{{"", home}, {other, other}, {home, home}} {
		opened, err := c.OpenNewProjectDesign(ctx, tc.asked)
		fixturekit.Must(t, err)
		if opened.Design.Project != tc.host {
			t.Fatalf("asked %q: hosted in %s, want %s", tc.asked, opened.Design.Project, tc.host)
		}
	}
	ordinary, err := c.OpenDesign(ctx, other)
	fixturekit.Must(t, err)
	if len(ordinary.Design.Projects) != 1 || ordinary.Design.Projects[0] != other {
		t.Fatalf("ordinary design: %+v", ordinary.Design)
	}

	empty, _ := journey.ProjectFixture(t)
	_, none := journey.StartT(t, empty)
	_, err = none.OpenNewProjectDesign(ctx, "")
	intakefixture.ApiError(t, err, api.NoProject, "a new-project design session lives in an existing project, and no project is active; add one with osmia project add first")
	designs, err := none.Designs(ctx, "")
	fixturekit.Must(t, err)
	if len(designs.Designs) != 0 {
		t.Fatalf("a session opened without an active project: %+v", designs.Designs)
	}
}
