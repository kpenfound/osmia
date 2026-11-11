package browsertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The design form offers New project. Opened from a project's card it hosts
// the session in that project, and from the sidebar in the first active
// project. The page shows the project the agent drafted, with its visibility.
func TestBrowserDesignFormOpensANewProjectSession(t *testing.T) {
	p := openBrowser(t)
	s, c, home, engine := newDesignEnforcementFixture(t, true)
	ctx := context.Background()
	clone := filepath.Join(t.TempDir(), "client")
	demoGit(t, filepath.Dir(clone), "init", "--quiet", clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("client\n"), 0600))
	demoGit(t, filepath.Dir(clone), "-C", clone, "add", "README.md")
	demoGit(t, filepath.Dir(clone), "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	added, err := c.AddProject(ctx, ProjectAddRequest{Name: "Client", Upstream: "owner/client", Clone: clone})
	must(t, err)
	other := added.Project.ID
	writeCharter(t, c, other, "1. Keep the client compatible.\n")

	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		out, err := callTool(ctx, tools, draftBriefToolName, map[string]any{
			"content":     "Decisions: build a gadget.\nIntent and tradeoffs: small.\nRejected options: a monolith.",
			"new_project": map[string]any{"name": "Gadget Works", "repo": "gadget-works", "description": "Makes gadgets.", "charter": "1. Keep it small.\n"},
		})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("draft: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "new-project", ResultText: "Recorded."}, nil
	}
	engine.Mu.Unlock()

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)

	// From the sidebar, with no project chosen: the first active project hosts it.
	p.Click("#sidebar-new-design")
	p.AwaitText("#design-add-form", "New project")
	p.Click("#design-add-form [name=new_project]")
	p.Click("#design-add-form [type=submit]")
	p.AwaitText("#design-add-form .result", "Opened design session")
	p.Await("the design view shown", `!document.querySelector('[data-view="design"]').hidden`)
	listed, err := c.Designs(ctx, "")
	must(t, err)
	if len(listed.Designs) != 1 || listed.Designs[0].Project != home || len(listed.Designs[0].Projects) != 0 {
		t.Fatalf("sidebar-opened new-project design: %+v", listed.Designs)
	}
	first := listed.Designs[0].Workstream

	// From a project's card: that project hosts it.
	p.Click("#project-home")
	p.Click(`[data-project="` + string(other) + `"] [data-project-action="design-add"]`)
	p.Await("the design action selects its project", `document.querySelector('#design-add-form [data-project-select]').value === `+quote(string(other)))
	p.Click("#design-add-form [name=new_project]")
	p.Click("#design-add-form [type=submit]")
	p.AwaitText("#design-add-form .result", "Opened design session")
	listed, err = c.Designs(ctx, other)
	must(t, err)
	if len(listed.Designs) != 1 || listed.Designs[0].Project != other || listed.Designs[0].Workstream == first {
		t.Fatalf("card-opened new-project design: %+v", listed.Designs)
	}
	second := listed.Designs[0].Workstream

	// The drafted new project is shown before Start, private by default.
	p.Await("the second session shown", fmt.Sprintf(`JSON.parse(localStorage.getItem('osmia.selected')) === %q`, second))
	p.TypeInto("#design-form [name=text]", "Draft the gadget project.")
	p.Click("#design-form [type=submit]")
	p.AwaitText("#design-brief", "New project: Gadget Works")
	p.AwaitText("#design-brief", "Repository: gadget-works")
	p.AwaitText("#design-brief", "Visibility: private")
	p.AwaitText("#design-brief", "Description: Makes gadgets.")
	view, err := c.Design(ctx, config.WorkstreamID(second))
	must(t, err)
	if view.Design.NewProject == nil || view.Design.NewProject.Visibility != "private" {
		t.Fatalf("design view: %+v", view.Design)
	}
}

// With no active project the New project form refuses with a clear message
// and opens no session.
func TestBrowserDesignFormRefusesANewProjectWithoutAnActiveProject(t *testing.T) {
	p := openBrowser(t)
	opts, _ := projectFixture(t)
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(data, []byte(listenWeb)...), 0600))
	s, c := start(t, opts)

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Click("#sidebar-new-design")
	p.Click("#design-add-form [name=new_project]")
	p.Click("#design-add-form [type=submit]")
	p.AwaitText("#design-add-form .result", "no project is active")
	p.Await("an error outcome", `document.querySelector('#design-add-form .result').dataset.outcome === 'error'`)
	listed, err := c.Designs(context.Background(), "")
	must(t, err)
	if len(listed.Designs) != 0 {
		t.Fatalf("a session opened without an active project: %+v", listed.Designs)
	}
}
