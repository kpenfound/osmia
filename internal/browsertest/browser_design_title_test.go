package browsertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	designTitleFirst   = "Plan the resumable upload export"
	designTitleSecond  = "Plan resumable uploads for the server and the client"
	designTitleHandoff = "Resumable uploads across the server and the client"
)

// TestBrowserDesignTitlesFollowTheAgentAndTheHandoffTitlesTheStartedWork runs
// the real service with fake agents. A design turn calls the title tool and
// the page shows that title, in place of the workstream ID, in the sidebar
// row, the project pane's design-session list and the design view; a second
// call updates all three without a reload. The owner starts the work from the
// page, the hand-off chief-of-staff turn sets the status goal with
// set_status, and the page then shows that goal as the title of the started
// workstream or workgroup.
func TestBrowserDesignTitlesFollowTheAgentAndTheHandoffTitlesTheStartedWork(t *testing.T) {
	for _, workgroup := range []bool{false, true} {
		name := "sole member"
		if workgroup {
			name = "workgroup"
		}
		t.Run(name, func(t *testing.T) {
			browserDesignTitles(t, workgroup)
		})
	}
}

func browserDesignTitles(t *testing.T, workgroup bool) {
	t.Helper()
	p := openBrowser(t)
	s, c, home, engine := newDesignEnforcementFixture(t, true)
	ctx := context.Background()
	projects := []config.ProjectID{home}
	if workgroup {
		clone := filepath.Join(t.TempDir(), "client")
		demoGit(t, filepath.Dir(clone), "init", "--quiet", clone)
		must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("client\n"), 0600))
		demoGit(t, filepath.Dir(clone), "-C", clone, "add", "README.md")
		demoGit(t, filepath.Dir(clone), "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
		added, err := c.AddProject(ctx, ProjectAddRequest{Name: "Client", Upstream: "owner/client", Clone: clone})
		must(t, err)
		writeCharter(t, c, added.Project.ID, "1. Keep the client compatible.\n")
		projects = append(projects, added.Project.ID)
	}
	const brief = "Decisions: resume uploads from the acknowledged offset.\nIntent and tradeoffs: favor simplicity.\nRejected options: a separate upload service."

	var mu sync.Mutex
	var problems []string
	problem := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	var designTurns, handoffTurns atomic.Int32
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		listed, err := tools.ListTools(ctx, nil)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(listed.Tools))
		for _, tool := range listed.Tools {
			names = append(names, tool.Name)
		}
		if !slices.Contains(names, "set_title") {
			// The hand-off turn of the chief of staff: it titles the started
			// work through its status goal.
			if !strings.Contains(req.Prompt, "Title the started work") {
				return &agent.Result{ClaudeID: "chief", ResultText: "Nothing to do."}, nil
			}
			handoffTurns.Add(1)
			input := map[string]any{"goal": designTitleHandoff, "note": "The work is titled from the handed brief.", "agents": []string{}}
			out, err := callTool(ctx, tools, "set_status", input)
			if err == nil && strings.Contains(out, "attention is required") {
				input["attention"] = "Review what the owner holds."
				out, err = callTool(ctx, tools, "set_status", input)
			}
			if err != nil || !strings.Contains(out, `"stored":true`) {
				problem("set_status: %s %v", out, err)
			}
			return &agent.Result{ClaudeID: "chief", ResultText: "Titled."}, nil
		}
		title := designTitleFirst
		if designTurns.Add(1) > 1 {
			title = designTitleSecond
		}
		if title == designTitleFirst {
			out, err := callTool(ctx, tools, "draft_brief", map[string]any{"content": brief, "projects": projects})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				problem("draft_brief: %s %v", out, err)
			}
		}
		out, err := callTool(ctx, tools, "set_title", map[string]any{"title": title})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			problem("set_title: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "design", ResultText: "Titled the session."}, nil
	}
	engine.Mu.Unlock()

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Click("#sidebar-new-design")
	p.Choose("#design-add-form [data-project-select]", string(home))
	p.Click("#design-add-form [type=submit]")
	p.Await("the design view", `!document.querySelector('[data-view="design"]').hidden`)
	listed, err := c.Designs(ctx, home)
	must(t, err)
	if len(listed.Designs) != 1 {
		t.Fatalf("designs: %+v", listed)
	}
	id := listed.Designs[0].Workstream
	sidebar := `#workstream-list [data-select="` + string(id) + `"] [data-field=goal]`
	pane := `#project-pane [data-pane-workstream="` + string(id) + `"] strong`

	// shows checks the three places a design session is listed or shown.
	shows := func(label, want string) {
		t.Helper()
		p.Await("the sidebar row labelled "+label, textOf(sidebar)+` === `+quote(want))
		p.Await("the design view headed "+label, textOf("#design-title")+` === `+quote(want))
		p.Click("#project-home")
		p.Await("the project pane", `!document.querySelector('[data-view="project-pane"]').hidden`)
		p.Await("the project pane's design list labelled "+label, textOf(pane)+` === `+quote(want))
		p.Click(`[data-drilldown="` + string(id) + `"]`)
		p.Await("the design view again", `!document.querySelector('[data-view="design"]').hidden`)
	}
	// An untitled session keeps its ID.
	shows("by its ID", string(id))

	// The agent sets the title; the page follows without a reload.
	p.Eval(`window.notReloaded = true`, nil)
	p.TypeInto("#design-form [name=text]", "We need resumable uploads for the server and the client.")
	p.Click("#design-form [type=submit]")
	p.AwaitText("#design-feed", "Titled the session.")
	shows("with the first title", designTitleFirst)

	// A second title call replaces it, again without a reload.
	p.TypeInto("#design-form [name=text]", "The client is in scope too, so widen the subject.")
	p.Click("#design-form [type=submit]")
	shows("with the second title", designTitleSecond)
	p.Await("no reload", `window.notReloaded === true`)

	// The owner starts the work from the page.
	awaitBrowserDesignBrief(t, p, c, id)
	p.Click("#design-start")
	p.Await("workgroup start", `document.getElementById('design-start-result').textContent.includes('Started a workgroup')`)
	if !workgroup {
		assertBrowserDesignSelection(t, p, c, id, true)
	}

	// The hand-off turn of the chief of staff sets the status goal, which the
	// started work shows through its goal display.
	if workgroup {
		var group config.WorkgroupID
		for end := time.Now().Add(browserTimeout); group == "" && time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			w, err := c.Status(ctx, id)
			must(t, err)
			if w.Workgroup != nil && len(w.Workgroup.Members) > 1 {
				group = w.Workgroup.ID
			}
		}
		if group == "" {
			t.Fatal("the started work reports no workgroup")
		}
		p.Await("the workgroup titled by the hand-off status in the sidebar", textOf(`#workstream-list [data-sidebar-workgroup="`+string(group)+`"] .sidebar-workgroup-title .title`)+` === `+quote(designTitleHandoff))
		p.Click(`[data-select-workgroup="` + string(group) + `"]`)
		p.Await("the workgroup view titled by the hand-off status", textOf("#workgroup-goal")+` === `+quote(designTitleHandoff))
	} else {
		p.Await("the started workstream titled by the hand-off status in the sidebar", textOf(`#workstream-list [data-select="`+string(id)+`"] [data-field=goal]`)+` === `+quote(designTitleHandoff))
		p.Await("the started workstream's goal display", textOf(`#workstream-head [data-field=goal]`)+` === `+quote(designTitleHandoff))
		p.Click("#project-home")
		p.Await("the project pane", `!document.querySelector('[data-view="project-pane"]').hidden`)
		p.Await("the project pane lists the started work by its goal", textOf(pane)+` === `+quote(designTitleHandoff))
	}
	if got := handoffTurns.Load(); got != 1 {
		t.Fatalf("%d hand-off turns reached the chief of staff, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(problems) > 0 {
		t.Fatalf("scripted turn problems:\n%s", strings.Join(problems, "\n"))
	}
}
