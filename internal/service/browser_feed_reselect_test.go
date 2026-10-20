package service

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBrowserDesignConversationSurvivesReselection(t *testing.T) {
	t.Parallel()
	s, c, project, engine := newDesignEnforcementFixture(t, true)
	const reply = "Keep the endpoint compatible."
	const assumption = "Use existing naming conventions."
	engine.turns["*"] = func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "design", ResultText: reply + "\n<details><summary>Assumptions</summary>" + assumption + "</details>"}, nil
	}
	ctx := context.Background()
	clone := s.current().For(project).Project.Clone
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
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.eval(`window.notReloaded = true`, nil)
	p.click("#sidebar-new-design")
	p.choose("#design-add-form [data-project-select]", string(project))
	p.click("#design-add-form [type=submit]")
	p.awaitText("#design-add-form .result", "Opened design session")
	p.await("the design view", `!document.querySelector('[data-view="design"]').hidden`)
	listed, err := c.Designs(ctx, project)
	must(t, err)
	if len(listed.Designs) != 1 {
		t.Fatalf("designs: %+v", listed)
	}
	id := listed.Designs[0].Workstream
	const message = "Preserve compatibility when switching workstreams."
	p.typeInto("#design-form [name=text]", message)
	p.click("#design-form [type=submit]")
	p.awaitText("#design-result", "the design agent answers in its next turn")
	p.awaitText("#design-feed", message)
	p.awaitText("#design-feed", reply)
	p.awaitText("#design-feed details", assumption)
	p.click("#design-feed summary")
	p.eval(`window.designEntry = document.querySelector('#design-feed > ol > li')`, nil)
	p.selectDesign(id)
	p.await("the unchanged feed keeps its nodes and expanded assumptions", `document.querySelector('#design-feed > ol > li') === window.designEntry && document.querySelector('#design-feed details').open`)

	other, err := c.OpenDesign(ctx, project)
	must(t, err)
	const otherMessage = "A separate design conversation."
	_, err = c.DesignMessage(ctx, other.Design.Workstream, otherMessage)
	must(t, err)
	awaitDesignConversation(t, c, other.Design.Workstream, func(r ConversationResponse) bool {
		return len(r.Entries) == 2 && r.Entries[1].State == TurnDone
	})

	for _, target := range []string{"workstream", "session", "design", "project"} {
		t.Logf("returning from %s", target)
		switch target {
		case "workstream":
			p.selectWorkstream(work.Workstream)
		case "session":
			p.selectSession(session.Session.Workstream)
		case "design":
			p.selectDesign(other.Design.Workstream)
			p.awaitText("#design-feed", otherMessage)
			assertFeedExcludes(t, p, "#design-feed", message)
		case "project":
			p.click("#project-home")
			p.await("the project pane", `!document.querySelector('[data-view="project-pane"]').hidden`)
		}
		if target != "design" && target != "project" {
			p.awaitText("#design-feed", "No design session selected.")
			assertFeedExcludes(t, p, "#design-feed", message)
		}
		if target != "session" {
			p.awaitText("#session-feed", "No session selected.")
		}
		if target == "project" {
			p.click(`[data-drilldown="` + string(id) + `"]`)
		} else {
			p.selectDesign(id)
		}
		p.await("the design view on return", `!document.querySelector('[data-view="design"]').hidden`)
		p.awaitText("#design-feed", message)
		p.awaitText("#design-feed", reply)
		if target != "project" {
			p.await("collapsed assumptions on return", `document.querySelector('#design-feed details') && !document.querySelector('#design-feed details').open`)
			p.click("#design-feed summary")
		}
		p.await("expanded assumptions", `document.querySelector('#design-feed details').open`)
		p.awaitText("#design-feed details", assumption)
		assertFeedExcludes(t, p, "#design-feed", otherMessage)
	}

	p.selectWorkstream(work.Workstream)
	const whileAway = "Include the entry recorded while I was away."
	_, err = c.DesignMessage(ctx, id, whileAway)
	must(t, err)
	conversation := awaitDesignConversation(t, c, id, func(r ConversationResponse) bool {
		return len(r.Entries) == 4 && r.Entries[3].State == TurnDone
	})
	p.selectDesign(id)
	p.await("the design view on return", `!document.querySelector('[data-view="design"]').hidden`)
	p.awaitText("#design-feed", message)
	p.awaitText("#design-feed", whileAway)
	p.await("the entire current design conversation", `document.querySelectorAll('#design-feed > ol > li').length === `+strconv.Itoa(len(conversation.Entries)))
	p.await("the same document", `window.notReloaded === true`)
}

func TestBrowserSessionConversationSurvivesReselection(t *testing.T) {
	t.Parallel()
	s, c, project, engine, _ := sessionWebFixture(t)
	engine.turn = func(context.Context, agent.Request) (*agent.Result, error) {
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
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.eval(`window.notReloaded = true`, nil)
	p.selectSession(id)
	const message = "Keep my exploratory conversation visible."
	p.typeInto("#session-form textarea", message)
	p.click("#session-form [type=submit]")
	p.awaitText("#session-feed", message)
	p.awaitText("#session-feed", "The session agent replied.")

	p.selectWorkstream(work.Workstream)
	p.awaitText("#session-feed", "No session selected.")
	assertFeedExcludes(t, p, "#session-feed", message)
	p.selectSession(id)
	p.awaitText("#session-feed", message)
	p.awaitText("#session-feed", "The session agent replied.")

	p.selectSession(other.Session.Workstream)
	p.typeInto("#session-form textarea", otherMessage)
	p.click("#session-form [type=submit]")
	p.awaitText("#session-feed", otherMessage)
	p.awaitText("#session-feed", "The session agent replied.")
	assertFeedExcludes(t, p, "#session-feed", message)
	p.selectSession(id)
	p.awaitText("#session-feed", message)
	assertFeedExcludes(t, p, "#session-feed", otherMessage)

	p.click("#project-home")
	p.await("the project pane", `!document.querySelector('[data-view="project-pane"]').hidden`)
	p.click(`[data-drilldown="` + string(id) + `"]`)
	p.await("the session view on return", `!document.querySelector('[data-view="session"]').hidden`)
	p.awaitText("#session-feed", message)
	p.awaitText("#session-feed", "The session agent replied.")
	p.await("the entire current session conversation", `document.querySelectorAll('#session-feed > ol > li').length === 2`)
	p.awaitText("#design-feed", "No design session selected.")
	assertFeedExcludes(t, p, "#session-feed", otherMessage)
	p.await("the same document", `window.notReloaded === true`)
}

func assertFeedExcludes(t *testing.T, p *page, selector, text string) {
	t.Helper()
	var contains bool
	p.eval(textOf(selector)+".includes("+quote(text)+")", &contains)
	if contains {
		t.Fatalf("%s shows another selection's entry %q", selector, text)
	}
}
