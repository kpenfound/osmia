package browsertest

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBrowserDesignConversationSurvivesReselection(t *testing.T) {
	t.Parallel()
	s, c, project, engine := newDesignEnforcementFixture(t, true)
	const reply = "Keep the endpoint compatible."
	const assumption = "Use existing naming conventions."
	engine.Turns["*"] = func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "design", ResultText: reply + "\n<details><summary>Assumptions</summary>" + assumption + "</details>"}, nil
	}
	ctx := context.Background()
	clone := service.InspectForTest(s).Config().For(project).Project.Clone
	home := filepath.Dir(clone)
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	demoGit(t, home, "-C", clone, "branch", "-M", "main")
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	demoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	brief := "An ordinary workstream to visit."
	work, err := c.HandIn(ctx, HandInRequest{Project: project, Key: "feed-reselect", Stdin: &brief})
	must(t, err)
	session, err := c.OpenSession(ctx, SessionOpenRequest{Project: project, Profile: "default"})
	must(t, err)

	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Eval(`window.notReloaded = true`, nil)
	p.Click("#sidebar-new-design")
	p.Choose("#design-add-form [data-project-select]", string(project))
	p.Click("#design-add-form [type=submit]")
	p.AwaitText("#design-add-form .result", "Opened design session")
	p.Await("the design view", `!document.querySelector('[data-view="design"]').hidden`)
	listed, err := c.Designs(ctx, project)
	must(t, err)
	if len(listed.Designs) != 1 {
		t.Fatalf("designs: %+v", listed)
	}
	id := listed.Designs[0].Workstream
	const message = "Preserve compatibility when switching workstreams."
	p.TypeInto("#design-form [name=text]", message)
	p.Click("#design-form [type=submit]")
	p.AwaitText("#design-result", "the design agent answers in its next turn")
	p.AwaitText("#design-feed", message)
	p.AwaitText("#design-feed", reply)
	p.AwaitText("#design-feed details", assumption)
	p.Click("#design-feed summary")
	p.Eval(`window.designEntry = document.querySelector('#design-feed > ol > li')`, nil)
	p.SelectDesign(id)
	p.Await("the unchanged feed keeps its nodes and expanded assumptions", `document.querySelector('#design-feed > ol > li') === window.designEntry && document.querySelector('#design-feed details').open`)

	other, err := c.OpenDesign(ctx, project)
	must(t, err)
	const otherMessage = "A separate design conversation."
	_, err = c.DesignMessage(ctx, other.Design.Workstream, otherMessage)
	must(t, err)
	awaitDesignConversation(t, s, c, other.Design.Workstream, func(r ConversationResponse) bool {
		return len(r.Entries) == 2 && r.Entries[1].State == TurnDone
	})

	for _, target := range []string{"workstream", "session", "design", "project"} {
		t.Logf("returning from %s", target)
		switch target {
		case "workstream":
			p.SelectWorkstream(work.Workstream)
		case "session":
			p.SelectSession(session.Session.Workstream)
		case "design":
			p.SelectDesign(other.Design.Workstream)
			p.AwaitText("#design-feed", otherMessage)
			assertFeedExcludes(t, p, "#design-feed", message)
		case "project":
			p.Click("#project-home")
			p.Await("the project pane", `!document.querySelector('[data-view="project-pane"]').hidden`)
		}
		if target != "design" && target != "project" {
			p.AwaitText("#design-feed", "No design session selected.")
			assertFeedExcludes(t, p, "#design-feed", message)
		}
		if target != "session" {
			p.AwaitText("#session-feed", "No session selected.")
		}
		if target == "project" {
			p.Click(`[data-drilldown="` + string(id) + `"]`)
		} else {
			p.SelectDesign(id)
		}
		p.Await("the design view on return", `!document.querySelector('[data-view="design"]').hidden`)
		p.AwaitText("#design-feed", message)
		p.AwaitText("#design-feed", reply)
		if target != "project" {
			p.Await("collapsed assumptions on return", `document.querySelector('#design-feed details') && !document.querySelector('#design-feed details').open`)
			p.Click("#design-feed summary")
		}
		p.Await("expanded assumptions", `document.querySelector('#design-feed details').open`)
		p.AwaitText("#design-feed details", assumption)
		assertFeedExcludes(t, p, "#design-feed", otherMessage)
	}

	p.SelectWorkstream(work.Workstream)
	const whileAway = "Include the entry recorded while I was away."
	_, err = c.DesignMessage(ctx, id, whileAway)
	must(t, err)
	conversation := awaitDesignConversation(t, s, c, id, func(r ConversationResponse) bool {
		return len(r.Entries) == 4 && r.Entries[3].State == TurnDone
	})
	p.SelectDesign(id)
	p.Await("the design view on return", `!document.querySelector('[data-view="design"]').hidden`)
	p.AwaitText("#design-feed", message)
	p.AwaitText("#design-feed", whileAway)
	p.Await("the entire current design conversation", `document.querySelectorAll('#design-feed > ol > li').length === `+strconv.Itoa(len(conversation.Entries)))
	p.Await("the same document", `window.notReloaded === true`)
}

func TestBrowserSessionConversationSurvivesReselection(t *testing.T) {
	t.Parallel()
	s, c, project, engine, _ := sessionWebFixture(t)
	engine.Turn = func(context.Context, agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "session", ResultText: "The session agent replied."}, nil
	}
	ctx := context.Background()
	brief := "An ordinary workstream to visit."
	work, err := c.HandIn(ctx, HandInRequest{Project: project, Key: "session-feed-reselect", Stdin: &brief})
	must(t, err)
	opened, err := c.OpenSession(ctx, SessionOpenRequest{Project: project, Profile: "default"})
	must(t, err)
	id := opened.Session.Workstream
	other, err := c.OpenSession(ctx, SessionOpenRequest{Project: project, Profile: "default"})
	must(t, err)
	const otherMessage = "A separate exploratory conversation."

	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Eval(`window.notReloaded = true`, nil)
	p.SelectSession(id)
	const message = "Keep my exploratory conversation visible."
	p.TypeInto("#session-form textarea", message)
	p.Click("#session-form [type=submit]")
	p.AwaitText("#session-feed", message)
	p.AwaitText("#session-feed", "The session agent replied.")

	p.SelectWorkstream(work.Workstream)
	p.AwaitText("#session-feed", "No session selected.")
	assertFeedExcludes(t, p, "#session-feed", message)
	p.SelectSession(id)
	p.AwaitText("#session-feed", message)
	p.AwaitText("#session-feed", "The session agent replied.")

	p.SelectSession(other.Session.Workstream)
	p.TypeInto("#session-form textarea", otherMessage)
	p.Click("#session-form [type=submit]")
	p.AwaitText("#session-feed", otherMessage)
	p.AwaitText("#session-feed", "The session agent replied.")
	assertFeedExcludes(t, p, "#session-feed", message)
	p.SelectSession(id)
	p.AwaitText("#session-feed", message)
	assertFeedExcludes(t, p, "#session-feed", otherMessage)

	p.Click("#project-home")
	p.Await("the project pane", `!document.querySelector('[data-view="project-pane"]').hidden`)
	p.Click(`[data-drilldown="` + string(id) + `"]`)
	p.Await("the session view on return", `!document.querySelector('[data-view="session"]').hidden`)
	p.AwaitText("#session-feed", message)
	p.AwaitText("#session-feed", "The session agent replied.")
	p.Await("the entire current session conversation", `document.querySelectorAll('#session-feed > ol > li').length === 2`)
	p.AwaitText("#design-feed", "No design session selected.")
	assertFeedExcludes(t, p, "#session-feed", otherMessage)
	p.Await("the same document", `window.notReloaded === true`)
}

func assertFeedExcludes(t *testing.T, p *page, selector, text string) {
	t.Helper()
	var contains bool
	p.Eval(textOf(selector)+".includes("+quote(text)+")", &contains)
	if contains {
		t.Fatalf("%s shows another selection's entry %q", selector, text)
	}
}
