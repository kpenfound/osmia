package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cdpruntime "github.com/chromedp/cdproto/runtime"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// openOutcomePage holds only explicitly named responses, after the real
// service has answered. Each URL has its own release, so simultaneous sends
// can complete independently. The page's EventSource remains the refresh path.
func openOutcomePage(t *testing.T, s *Service) *page {
	t.Helper()
	p := openBrowser(t)
	p.run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(`
   window.sameOutcomeDocument = true;
   window.setInterval = () => 0;
   window.outcomeEvents = [];
   const Events = window.EventSource;
   window.EventSource = class extends Events {
    constructor(...args) {
     super(...args);
     this.addEventListener('workstream', event => outcomeEvents.push(JSON.parse(event.data)));
    }
   };
   window.holdOutcome = new Set();
   window.releaseOutcome = {};
   window.outcomeResponses = {};
   window.outcomeRequests = {};
   const serviceFetch = window.fetch.bind(window);
   window.fetch = async (url, init) => {
    const held = holdOutcome.delete(url);
    if (held) outcomeRequests[url] = (outcomeRequests[url] || 0) + 1;
    const response = await serviceFetch(url, init);
    if (held) {
     outcomeResponses[url] = await response.clone().json();
     await new Promise(resolve => releaseOutcome[url] = resolve);
    }
    return response;
   };`).Do(ctx)
		return err
	}), chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	return p
}

func assertOutcome(p *page, selector, state, text string) {
	p.t.Helper()
	p.await("the exact outcome in "+selector, `document.querySelector(`+quote(selector)+`).dataset.outcome === `+quote(state)+` && `+textOf(selector)+` === `+quote(text))
}

func holdOutcome(p *page, url string) {
	p.t.Helper()
	p.eval(`delete releaseOutcome[`+quote(url)+`]; delete outcomeResponses[`+quote(url)+`]; holdOutcome.add(`+quote(url)+`)`, nil)
}

func releaseOutcome(p *page, url string) {
	p.t.Helper()
	p.await("the held response", `typeof releaseOutcome[`+quote(url)+`] === 'function'`)
	p.eval(`releaseOutcome[`+quote(url)+`]()`, nil)
}

func selectOutcomeDesign(p *page, id config.WorkstreamID) {
	p.t.Helper()
	row := `[data-select="` + string(id) + `"]`
	p.await("the design in the sidebar", `document.querySelector(`+quote(row)+`) !== null`)
	p.click(row)
	p.await("the selected design", `!document.querySelector('[data-view=design]').hidden && `+textOf("#design-title")+` === `+quote(string(id)))
}

// exerciseHeldSends checks both a completion while B is empty and a completion
// while B has its own pending send. It also checks ordinary reselection after
// completion, exact final text/state, and one dispatch per held action.
func exerciseHeldSends(p *page, a, b config.WorkstreamID, kind, form, result, success string, selectInstance func(*page, config.WorkstreamID)) {
	p.t.Helper()
	urlA := "/v1/" + kind + "/" + string(a) + "/message"
	urlB := "/v1/" + kind + "/" + string(b) + "/message"
	button := form + " [type=submit]"
	for round := range 2 {
		selectInstance(p, a)
		if kind == "sessions" {
			p.await("the originating session is idle", `document.querySelector('#session-meta [data-field=state]').textContent === 'idle'`)
		}
		holdOutcome(p, urlA)
		p.typeInto(form+" textarea", fmt.Sprintf("Message A %d", round))
		p.click(button)
		assertOutcome(p, result, "pending", "Sending…")
		selectInstance(p, b)
		assertOutcome(p, result, "", "")
		p.await("B's send remains enabled", `!document.querySelector(`+quote(button)+`).disabled`)
		if round == 1 {
			holdOutcome(p, urlB)
			p.typeInto(form+" textarea", "Message B")
			p.click(button)
			assertOutcome(p, result, "pending", "Sending…")
		}
		releaseOutcome(p, urlA)
		// Fetch completion precedes act's final paint; two animation frames let
		// the response continuation and the page's paint both finish.
		p.run(chromedp.Evaluate(`new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))`, nil, func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams { return p.WithAwaitPromise(true) }))
		if round == 0 {
			assertOutcome(p, result, "", "")
		} else {
			assertOutcome(p, result, "pending", "Sending…")
			p.await("A's completion cannot enable B", `document.querySelector(`+quote(button)+`).disabled`)
		}
		selectInstance(p, a)
		var state string
		p.eval(`outcomeResponses[`+quote(urlA)+`].state`, &state)
		assertOutcome(p, result, "ok", success+state+").")
		selectInstance(p, b)
		if round == 1 {
			releaseOutcome(p, urlB)
			p.eval(`outcomeResponses[`+quote(urlB)+`].state`, &state)
			assertOutcome(p, result, "ok", success+state+").")
			p.await("B's own completion enables Send", `!document.querySelector(`+quote(button)+`).disabled`)
		}
		selectInstance(p, a)
		p.eval(`outcomeResponses[`+quote(urlA)+`].state`, &state)
		assertOutcome(p, result, "ok", success+state+").")
	}
	p.await("each held action dispatched once", `outcomeRequests[`+quote(urlA)+`] === 2 && outcomeRequests[`+quote(urlB)+`] === 1 && sameOutcomeDocument`)
	p.await("real service events observed", `outcomeEvents.some(e => e.workstream === `+quote(string(a))+`)`)
}

func TestBrowserDesignOutcomesBelongToTheirSession(t *testing.T) {
	s, c, project, engine := newDesignEnforcementFixture(t, true)
	engine.turns["*"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := callTool(ctx, tools, draftBriefToolName, map[string]any{"content": "Decisions: keep compatibility.\nIntent and tradeoffs: prefer existing names.\nRejected options: a breaking replacement."})
		return &agent.Result{ClaudeID: "design", ResultText: "Brief ready."}, err
	}
	a, err := c.OpenDesign(context.Background(), project)
	must(t, err)
	b, err := c.OpenDesign(context.Background(), project)
	must(t, err)
	p := openOutcomePage(t, s)
	selectOutcomeDesign(p, a.Design.Workstream)
	p.click("#design-start")
	p.awaitText("#design-start-result[data-outcome=error]", "has not drafted a brief yet")
	var refusal string
	p.eval(textOf("#design-start-result"), &refusal)
	selectOutcomeDesign(p, b.Design.Workstream)
	assertOutcome(p, "#design-start-result", "", "")
	selectOutcomeDesign(p, a.Design.Workstream)
	assertOutcome(p, "#design-start-result", "error", refusal)
	exerciseHeldSends(p, a.Design.Workstream, b.Design.Workstream, "design", "#design-form", "#design-result", "Sent; the design agent answers in its next turn (", selectOutcomeDesign)
	p.await("both design turns completed on the page", `document.querySelectorAll('#design-feed [data-kind=response][data-state=done]').length === 2`)
	p.awaitText("#design-feed", "Brief ready.")
	p.click("#design-start")
	p.awaitText("#design-start-result[data-outcome=ok]", "Started a workgroup from this brief")
	p.await("start still navigates to its workstream", `!document.querySelector('[data-view=workstream]').hidden`)
	selectOutcomeDesign(p, b.Design.Workstream)
	assertOutcome(p, "#design-start-result", "", "")
	// Open a design through the page after the start, rather than reusing a
	// fixture-only session, to cover the fresh create/navigation path too.
	p.click("#sidebar-new-design")
	p.choose("#design-add-form [name=project]", string(project))
	p.click("#design-add-form [type=submit]")
	p.awaitText("#design-add-form .result", "Opened design session")
	p.await("the new design is displayed", `!document.querySelector('[data-view=design]').hidden`)
	assertOutcome(p, "#design-start-result", "", "")
	assertOutcome(p, "#design-result", "", "")
}

func TestBrowserSessionOutcomesBelongToTheirSession(t *testing.T) {
	s, c, project, engine, _ := sessionWebFixture(t)
	engine.turn = func(ctx context.Context, req agent.Request) (*agent.Result, error) {
		if strings.Contains(req.Prompt, "Hold this turn") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &agent.Result{ClaudeID: "session", ResultText: "Session reply."}, nil
	}
	a, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: project, Profile: "default"})
	must(t, err)
	b, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: project, Profile: "default"})
	must(t, err)
	p := openOutcomePage(t, s)
	exerciseHeldSends(p, a.Session.Workstream, b.Session.Workstream, "sessions", "#session-form", "#session-result", "Sent; the agent answers in its next turn (", func(p *page, id config.WorkstreamID) { p.selectSession(id) })
	p.await("the session is idle", `document.querySelector('#session-meta [data-field=state]').textContent === 'idle'`)
	p.typeInto("#session-form textarea", "Hold this turn")
	p.click("#session-form [type=submit]")
	p.await("the stop control is enabled for a working turn", `!document.querySelector('#session-stop').disabled`)
	p.click("#session-stop")
	const stopped = "Stopped; the worktree keeps what the turn already changed."
	assertOutcome(p, "#session-control-result", "ok", stopped)
	p.selectSession(b.Session.Workstream)
	assertOutcome(p, "#session-control-result", "", "")
	p.selectSession(a.Session.Workstream)
	assertOutcome(p, "#session-control-result", "ok", stopped)
	p.await("the stopped turn has released the session", `document.querySelector('#session-meta [data-field=state]').textContent === 'idle'`)
	// Commit, check, push and end share the same selection contract, but keep
	// their separate outcome areas and their actual service effects.
	worktree := sessionWorktreePath(s.cfg.Root.String(), project, a.Session.Workstream)
	must(t, os.WriteFile(filepath.Join(worktree, "outcome.txt"), []byte("owner change\n"), 0600))
	p.typeInto("#session-commit-form [name=message]", "record the owner change")
	p.click("#session-commit-form [type=submit]")
	p.awaitText("#session-commit-form .result[data-outcome=ok]", "Committed")
	var commit string
	p.eval(textOf("#session-commit-form .result"), &commit)
	p.selectSession(b.Session.Workstream)
	assertOutcome(p, "#session-commit-form .result", "", "")
	p.selectSession(a.Session.Workstream)
	assertOutcome(p, "#session-commit-form .result", "ok", commit)
	for _, action := range []struct{ button, text string }{{"check", "Check run"}, {"push", "Pushed"}, {"end", "Session ended"}} {
		p.click("#session-" + action.button)
		p.awaitText("#session-control-result[data-outcome=ok]", action.text)
		var text string
		p.eval(textOf("#session-control-result"), &text)
		p.selectSession(b.Session.Workstream)
		assertOutcome(p, "#session-control-result", "", "")
		p.selectSession(a.Session.Workstream)
		assertOutcome(p, "#session-control-result", "ok", text)
	}
}

func TestBrowserWorkstreamOutcomesBelongToTheirWorkstream(t *testing.T) {
	f := newPageFixture(t)
	p := openOutcomePage(t, f.s)
	p.selectWorkstream(stream)
	// A real inbox answer uses the shared result moved between cards.
	live := f.s.sole().repository
	at := f.s.now()
	h := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: "outcome-mason", Revision: 1, Project: project, Workstream: stream, At: at, Actor: ownerActor, Cause: "fixture"}
	must(t, live.CreateThread(context.Background(), trace.Agent{Header: h, Role: demoRole, ThreadID: "outcome-thread"}))
	q, err := live.Ask(context.Background(), h.ID, claimTurn(t, live, t.TempDir(), stream, h.ID, "outcome-thread", "outcome-turn", at), "Which outcome belongs here?", at)
	must(t, err)
	_, err = live.EscalateQuestions(context.Background(), trace.ChiefOfStaff, f.chief, trace.EscalationRequest{Questions: []string{q.ID}, Rephrasing: "Which outcome belongs here?", Blocked: "The unit", Recommendation: "Keep it local."}, at)
	must(t, err)
	entries := entriesOf(t, f.c, InboxEscalation)
	if len(entries) != 1 {
		t.Fatalf("inbox: %+v", entries)
	}
	entry := entries[0]
	card := decisionCard(entry)
	url := entry.Answer.Path
	holdOutcome(p, url)
	p.awaitText(card+"[data-field=question]", "Which outcome belongs here?")
	p.typeInto(card+"textarea", "This workstream's outcome.")
	p.click(card + "[type=submit]")
	assertOutcome(p, "#inbox-result", "pending", "Sending…")
	p.selectWorkstream(quiet)
	assertOutcome(p, "#inbox-result", "", "")
	releaseOutcome(p, url)
	p.run(chromedp.Evaluate(`new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))`, nil, func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams { return p.WithAwaitPromise(true) }))
	assertOutcome(p, "#inbox-result", "", "")
	p.selectWorkstream(stream)
	expected := fmt.Sprintf("Answered inbox entry %d.", entry.Number)
	assertOutcome(p, "#inbox-result", "ok", expected)
	p.selectWorkstream(quiet)
	assertOutcome(p, "#inbox-result", "", "")
	p.selectWorkstream(stream)
	assertOutcome(p, "#inbox-result", "ok", expected)
	// Documents, dependency and action forms have independent outcome
	// areas. Use the feed tab for reselection so no new document read is
	// requested as part of selecting the next workstream.
	p.click("[data-tab=documents]")
	p.awaitText("#documents-form .result[data-outcome=ok]", "Documents loaded.")
	p.awaitText("#base-form .result[data-outcome=ok]", "loaded.")
	p.typeInto("#workstream-action [name=note]", "An owner objection on this workstream.")
	p.click("#workstream-action [type=submit]")
	p.await("the action answered", `['ok', 'error', 'unavailable'].includes(document.querySelector('#workstream-action .result').dataset.outcome)`)
	type outcome struct {
		Text  string `json:"text"`
		State string `json:"state"`
	}
	recorded := map[string]outcome{}
	for _, selector := range []string{"#documents-form .result", "#base-form .result", "#workstream-action .result"} {
		var out outcome
		p.eval(`({text: document.querySelector(`+quote(selector)+`).textContent, state: document.querySelector(`+quote(selector)+`).dataset.outcome})`, &out)
		recorded[selector] = out
	}
	p.click("[data-tab=feed]")
	p.selectWorkstream(quiet)
	for selector := range recorded {
		assertOutcome(p, selector, "", "")
	}
	p.selectWorkstream(stream)
	for selector, out := range recorded {
		assertOutcome(p, selector, out.State, out.Text)
	}
	// Trace reads retain their result on reselection too.
	p.click("[data-tab=trace]")
	p.click("#trace-form [type=submit]")
	assertOutcome(p, "#trace-form .result", "ok", "Trace loaded.")
	p.selectWorkstream(quiet)
	assertOutcome(p, "#trace-form .result", "", "")
	p.selectWorkstream(stream)
	assertOutcome(p, "#trace-form .result", "ok", "Trace loaded.")
	p.await("one inbox dispatch and the original document", `outcomeRequests[`+quote(url)+`] === 1 && sameOutcomeDocument`)
}

func TestBrowserWorkstreamOutcomesFollowArchiveFallback(t *testing.T) {
	f := newPageFixture(t)
	_, err := f.c.Abandon(context.Background(), quiet, "Finished with this work")
	must(t, err)
	p := openOutcomePage(t, f.s)
	p.selectWorkstream(quiet)
	p.click("[data-tab=trace]")
	p.click("#trace-form [type=submit]")
	assertOutcome(p, "#trace-form .result", "ok", "Trace loaded.")
	p.click("[data-tab=feed]")
	p.eval(`window.confirm = () => true`, nil)
	url := "/v1/archive/" + string(quiet)
	holdOutcome(p, url)
	p.click("#workstream-menu-button")
	p.click(`[data-workstream-action="archive"]`)
	p.await("the real archive response held", `typeof releaseOutcome[`+quote(url)+`] === 'function' && outcomeResponses[`+quote(url)+`].archived`)
	// Only the status event has moved the displayed workstream: the archive
	// callback has not yet selected its successor or produced its final text.
	p.await("the event-driven fallback", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(stream))+` && location.hash === `+quote("#workstream="+string(quiet)))
	for _, selector := range []string{"#inbox-result", "#documents-form .result", "#base-form .result", "#workstream-action .result", "#trace-form .result"} {
		assertOutcome(p, selector, "", "")
	}
	p.await("the fallback has no archive busy state", `!document.querySelector('[data-workstream-action=archive]').disabled && !document.querySelector('[data-workstream-action=archive]').dataset.busy`)
	// Actions taken on the fallback must use its identity too, both for the
	// shared inbox result and for the workstream forms.
	p.click("#workstream-menu-button")
	p.click(`[data-workstream-action="rebase"]`)
	p.await("the fallback menu action answered", `['ok', 'error', 'unavailable'].includes(document.querySelector('#inbox-result').dataset.outcome)`)
	var menuText, menuState string
	p.eval(textOf("#inbox-result"), &menuText)
	p.eval(`document.querySelector('#inbox-result').dataset.outcome`, &menuState)
	p.click("[data-tab=documents]")
	p.awaitText("#documents-form .result[data-outcome=ok]", "Documents loaded.")
	p.awaitText("#base-form .result[data-outcome=ok]", "loaded.")
	var documentsText, baseText string
	p.eval(textOf("#documents-form .result"), &documentsText)
	p.eval(textOf("#base-form .result"), &baseText)
	p.click("[data-tab=trace]")
	p.click("#trace-form [type=submit]")
	assertOutcome(p, "#trace-form .result", "ok", "Trace loaded.")
	p.click("[data-tab=feed]")
	assertFallbackOutcomes := func() {
		t.Helper()
		assertOutcome(p, "#inbox-result", menuState, menuText)
		assertOutcome(p, "#documents-form .result", "ok", documentsText)
		assertOutcome(p, "#base-form .result", "ok", baseText)
		assertOutcome(p, "#trace-form .result", "ok", "Trace loaded.")
	}
	assertFallbackOutcomes()
	releaseOutcome(p, url)
	p.await("the archive callback selected its successor", `location.hash === `+quote("#workstream="+string(stream)))
	assertFallbackOutcomes()
	p.selectWorkstream(stream)
	assertFallbackOutcomes()
	p.await("one archive dispatch and the original document", `outcomeRequests[`+quote(url)+`] === 1 && sameOutcomeDocument`)
}

func TestBrowserCreateOutcomesBelongToTheirOpeningAndProject(t *testing.T) {
	s, c, first, _, clone := sessionWebFixture(t)
	ctx := context.Background()
	secondClone := filepath.Join(filepath.Dir(clone), "other-project")
	demoGit(t, filepath.Dir(clone), "clone", "-q", "--no-hardlinks", clone, secondClone)
	otherRequest := request(secondClone)
	otherRequest.Name, otherRequest.Upstream = "Other project", "owner/other"
	added, err := c.AddProject(ctx, otherRequest)
	must(t, err)
	second := added.Project.ID
	writeCharter(t, c, second, "1. Keep changes small.\n")
	repoClone := filepath.Join(filepath.Dir(clone), "extra-repo")
	demoGit(t, filepath.Dir(clone), "init", "-q", repoClone)
	p := openOutcomePage(t, s)
	open := func(view string, project config.ProjectID) {
		t.Helper()
		p.click("#project-home")
		selector := `[data-project="` + string(project) + `"] [data-project-action="` + view + `"]`
		p.await("the project create action", `document.querySelector(`+quote(selector)+`) !== null`)
		p.click(selector)
	}
	for _, form := range []struct{ view, id, success string }{
		{"handin", "handin-form", "Handed in"},
		{"design-add", "design-add-form", "Opened design session"},
		{"session-add", "session-add-form", "Opened session"},
		{"repo-add", "repo-add", "Added repo"},
	} {
		open(form.view, first)
		result := "#" + form.id + " .result"
		assertOutcome(p, result, "", "")
		switch form.view {
		case "handin":
			p.typeInto("#handin-form [name=content]", "A feature with an outcome scoped to its project.")
		case "session-add":
			p.choose("#session-add-form [name=profile]", "default")
		case "repo-add":
			p.typeInto("#repo-add [name=upstream]", "owner/extra")
			p.typeInto("#repo-add [name=clone]", repoClone)
		}
		p.click("#" + form.id + " [type=submit]")
		p.awaitText(result+"[data-outcome=ok]", form.success)
		open(form.view, first)
		assertOutcome(p, result, "", "")
		open(form.view, second)
		assertOutcome(p, result, "", "")
	}
	// A refusal stays in the project where it was made, including when the
	// owner changes the select without closing the shared form.
	open("repo-add", first)
	p.typeInto("#repo-add [name=upstream]", "owner/duplicate")
	p.typeInto("#repo-add [name=clone]", repoClone)
	p.click("#repo-add [type=submit]")
	p.awaitText("#repo-add .result[data-outcome=error]", "already owns")
	p.choose("#repo-add [name=project]", string(second))
	assertOutcome(p, "#repo-add .result", "", "")
}
