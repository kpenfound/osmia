package browsertest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Requests still reach the service; only delivery of selected reads is held.
// A resync exercises unchanged rereads through the page's event listener.
func openFeedScrollPage(t *testing.T, s *service.Service, hash, held string) *page {
	t.Helper()
	p := openBrowser(t)
	p.Run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(`
   window.setInterval = () => 0;
   window.feedRenders = 0;
   window.feedPending = 0;
   window.feedHolds = new Set([` + quote(held) + `]);
   window.feedRelease = {};
   const Events = window.EventSource;
   window.EventSource = class extends Events {
    constructor(...args) { super(...args); window.feedEvents = this; }
   };
   const serviceFetch = window.fetch.bind(window);
   window.fetch = async (url, init) => {
    const hold = feedHolds.delete(url);
    feedPending++;
    try {
     const response = await serviceFetch(url, init);
     if (hold) await new Promise(resolve => feedRelease[url] = resolve);
     return response;
    } finally { feedPending--; }
   };
   document.addEventListener('DOMContentLoaded', () => {
    new MutationObserver(() => feedRenders++).observe(document.querySelector('#meters'), {childList:true});
   });`).Do(ctx)
		return err
	}), chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"+hash))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	return p
}

func releaseFeedRead(p *page, path string) {
	p.T.Helper()
	p.Await("the held feed read", `typeof feedRelease[`+quote(path)+`] === 'function'`)
	p.Eval(`window.beforeFeedRender = feedRenders; feedRelease[`+quote(path)+`](); delete feedRelease[`+quote(path)+`]`, nil)
	p.Await("the released read rendered", `feedRenders > beforeFeedRender && feedPending === 0`)
	settleWorkgroupAction(p)
}

func rereadFeed(p *page) {
	p.T.Helper()
	p.Eval(`window.beforeFeedRender = feedRenders; feedEvents.dispatchEvent(new MessageEvent('resync', {data:'{}'}))`, nil)
	p.Await("the unchanged service reads rendered", `feedRenders > beforeFeedRender && feedPending === 0`)
	settleWorkgroupAction(p)
}

func feedAtBottom(p *page, selector string) {
	p.T.Helper()
	p.Await("the newest entry and composer at the bottom of "+selector, `(() => {
  const feed = document.querySelector(`+quote(selector)+`);
  const last = feed?.querySelector(':scope > ol > li:last-child');
  const main = document.querySelector('#main').getBoundingClientRect();
  const end = feed?.closest('.thread').getBoundingClientRect();
  const rect = last?.getBoundingClientRect();
  return rect && end && Math.abs(end.bottom-main.bottom) <= 2 && rect.bottom <= main.bottom && rect.bottom > main.top;
 })()`)
}

func scrollFeedUp(p *page) {
	p.T.Helper()
	settleWorkgroupAction(p)
	p.Eval(`document.querySelector('#main').scrollTop = 140; window.feedScrollBefore = document.querySelector('#main').scrollTop`, nil)
	p.Await("an overflowing feed scrolled up", `feedScrollBefore === 140 && document.querySelector('#main').scrollHeight > document.querySelector('#main').clientHeight + 500`)
}

func feedScrollUnchanged(p *page) {
	p.T.Helper()
	settleWorkgroupAction(p)
	p.Await("the scroll position remains unchanged", `Math.abs(document.querySelector('#main').scrollTop-feedScrollBefore) <= 1`)
}

func seedScrollMessages(t *testing.T, c *service.Client, id config.WorkstreamID) {
	t.Helper()
	for n := range 14 {
		_, err := c.Send(context.Background(), id, fmt.Sprintf("History %d. %s", n, strings.Repeat("Keep the conversation readable. ", 10)))
		must(t, err)
	}
}

func addScrollFollowup(t *testing.T, f *workgroupPageFixture, g *trace.Workgroup, host config.WorkstreamID) {
	t.Helper()
	change := lifecycle.WorkgroupChange{ID: "scroll-followup-" + string(host), Host: host, Workgroup: g.ID,
		State: "presented", Request: lifecycle.ChangeRequest{Kind: "amend", Scope: "workgroup", Change: "Add a follow-up"},
		After: trace.ProjectSpec{Behavior: "Keep the contract", Slices: []trace.Slice{{Workstream: host, Spec: fixturekit.ValidSpec, Plan: fixturekit.ValidPlan}}}}
	_, err := service.InspectForTest(f.s).LifecycleController().CreateChangeFollowup(context.Background(), f.repo, g, f.repo,
		amendment.StreamImpact{Workstream: host, Action: amendment.FollowUp}, &change)
	must(t, err)
}

func TestBrowserWorkgroupFeedScroll(t *testing.T) {
	ctx := context.Background()
	f := newMemberPageFixture(t, false, false)
	addScrollFollowup(t, f, &f.group, stream)
	groups, _, err := trace.Workgroups(f.repo)
	must(t, err)
	second, ok := trace.WorkgroupOf(groups, quiet)
	if !ok {
		t.Fatal("quiet workstream has no workgroup")
	}
	addScrollFollowup(t, f, &second, quiet)
	seedScrollMessages(t, f.c, stream)
	seedScrollMessages(t, f.c, quiet)
	path := "/v1/workgroups/" + string(f.group.ID) + "/feed"
	p := openFeedScrollPage(t, f.s, "#workgroup="+string(f.group.ID), path)
	p.AwaitText("#workgroup-content .feed", "Reading the conversation")
	releaseFeedRead(p, path)
	p.AwaitText("#workgroup-content .feed", "History 13.")
	feedAtBottom(p, "#workgroup-content .feed")
	p.Eval(`document.querySelector('#main').scrollTop = 0`, nil)
	p.Click(`[data-select-workgroup="` + string(second.ID) + `"]`)
	p.Await("the second multi-member group", `document.querySelector('#workgroup-content [data-workgroup]')?.dataset.workgroup === `+quote(string(second.ID))+` && document.querySelectorAll('#workgroup-content [data-member]').length === 2`)
	feedAtBottom(p, "#workgroup-content .feed")
	scrollFeedUp(p)
	rereadFeed(p)
	feedScrollUnchanged(p)

	// An actual conversation SSE entry snaps without changing the draft or focus.
	p.TypeInto("#workgroup-content .composer textarea", "Unsent group draft")
	p.Eval(`document.querySelector('#workgroup-content textarea').focus({preventScroll:true}); document.querySelector('#main').scrollTop = 0`, nil)
	_, err = f.c.Send(ctx, quiet, "Newest shared entry")
	must(t, err)
	p.AwaitText("#workgroup-content .feed", "Newest shared entry")
	feedAtBottom(p, "#workgroup-content .feed")
	p.Await("the group composer retains focus and text", `document.activeElement === document.querySelector('#workgroup-content textarea') && document.activeElement.value === 'Unsent group draft'`)
	scrollFeedUp(p)
	_, err = f.c.Send(ctx, stream, "An entry in the hidden group")
	must(t, err)
	p.Await("the hidden group's refreshed entry", `feedPending === 0 && `+textOf(`[data-select-workgroup="`+string(f.group.ID)+`"]`)+`.length > 0`)
	rereadFeed(p)
	feedScrollUnchanged(p)

	// Member links and project-pane drilldowns also reopen at the newest entry.
	p.Click(`[data-open-member="` + string(quiet) + `"]`)
	p.Click("#workstream-head [data-ask-chief]")
	feedAtBottom(p, "#workgroup-content .feed")
	p.Click("#project-home")
	p.Click(`[data-open-workgroup="` + string(f.group.ID) + `"]`)
	feedAtBottom(p, "#workgroup-content .feed")
}

func TestBrowserWorkstreamFeedScroll(t *testing.T) {
	f := newPageFixture(t)
	seedScrollMessages(t, f.c, stream)
	path := "/v1/feed/" + string(stream)
	p := openFeedScrollPage(t, f.s, "#workstream="+string(stream), path)
	p.AwaitText("#workstreams .feed", "Reading the feed")
	releaseFeedRead(p, path)
	p.AwaitText("#workstreams .feed", "History 13.")
	feedAtBottom(p, "#workstreams .feed")
	scrollFeedUp(p)
	rereadFeed(p)
	feedScrollUnchanged(p)
	p.TypeInto("#workstreams .composer textarea", "Unsent workstream draft")
	p.Eval(`document.querySelector('#workstreams textarea').focus({preventScroll:true}); document.querySelector('#main').scrollTop = 0`, nil)
	_, err := f.c.Send(context.Background(), stream, "Newest workstream entry")
	must(t, err)
	p.AwaitText("#workstreams .feed", "Newest workstream entry")
	feedAtBottom(p, "#workstreams .feed")
	p.Await("the workstream composer retains focus and text", `document.activeElement === document.querySelector('#workstreams textarea') && document.activeElement.value === 'Unsent workstream draft'`)
	for _, tab := range []string{"documents", "trace"} {
		scrollFeedUp(p)
		p.Click(`[data-tab="` + tab + `"]`)
		p.Await("the non-feed tab", `!document.querySelector('[data-panel="`+tab+`"]')?.hidden`)
		if tab == "trace" {
			p.Click("#trace-form [type=submit]")
			p.AwaitText("#trace-form .result", "Trace loaded")
		}
		p.Await("the non-feed reads completed", `feedPending === 0`)
		scrollFeedUp(p)
		_, err = f.c.Send(context.Background(), stream, "Hidden feed entry on "+tab)
		must(t, err)
		rereadFeed(p)
		feedScrollUnchanged(p)
		p.Click(`[data-tab=feed]`)
		feedAtBottom(p, "#workstreams .feed")
	}
	f.status(t, trace.StatusContent{Goal: "Project history " + strings.Repeat("Keep uploads working. ", 300), Note: "The project remains active.", Agents: []string{}})
	p.AwaitText("#workstream-head", "Project history")
	p.Click("#project-home")
	scrollFeedUp(p)
	_, err = f.c.Send(context.Background(), stream, "Hidden feed entry in project pane")
	must(t, err)
	rereadFeed(p)
	feedScrollUnchanged(p)
	p.Click(`[data-drilldown="` + string(stream) + `"]`)
	feedAtBottom(p, "#workstreams .feed")
	p.SelectWorkstream(quiet)
	p.SelectWorkstream(stream)
	feedAtBottom(p, "#workstreams .feed")
}

const scrollReply = "A detailed agent reply.\n\n"

func TestBrowserSessionFeedScroll(t *testing.T) {
	ctx := context.Background()
	s, c, project, engine, _ := sessionWebFixture(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	engine.Turn = func(ctx context.Context, req agent.Request) (*agent.Result, error) {
		if strings.Contains(req.Prompt, "Newest exploratory message") {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &agent.Result{ClaudeID: "session", ResultText: "Newest exploratory answer"}, nil
		}
		return &agent.Result{ClaudeID: "session", ResultText: strings.Repeat(scrollReply, 45)}, nil
	}
	opened, err := c.OpenSession(ctx, SessionOpenRequest{Project: project, Profile: "default"})
	must(t, err)
	id := opened.Session.Workstream
	p := openFeedScrollPage(t, s, "", "")
	p.SelectSession(id)
	p.TypeInto("#session-form textarea", "Seed exploratory history")
	p.Click("#session-form [type=submit]")
	p.AwaitText("#session-feed", scrollReply[:22])
	p.Await("the session is idle", textOf("#session-meta [data-field=state]")+` === 'idle' && feedPending === 0`)
	p.Click("#project-home")
	p.Click(`[data-drilldown="` + string(id) + `"]`)
	feedAtBottom(p, "#session-feed")
	scrollFeedUp(p)
	rereadFeed(p)
	feedScrollUnchanged(p)

	path := "/v1/feed/" + string(id)
	p.TypeInto("#session-form textarea", "Newest exploratory message")
	p.Eval(`feedHolds.add(`+quote(path)+`); document.querySelector('#session-form textarea').focus({preventScroll:true}); document.querySelector('#main').scrollTop = 0; document.querySelector('#session-form').requestSubmit()`, nil)
	p.AwaitText("#session-result", "Sent;")
	p.AwaitText("#session-feed", "Newest exploratory message")
	p.Await("the follow-up read is held", `typeof feedRelease[`+quote(path)+`] === 'function'`)
	feedAtBottom(p, "#session-feed")
	scrollFeedUp(p)
	releaseFeedRead(p, path)
	feedScrollUnchanged(p)
	rereadFeed(p)
	feedScrollUnchanged(p)
	// The agent's later response is another new entry, with a draft in progress.
	p.TypeInto("#session-form textarea", "Unsent exploratory draft")
	p.Eval(`document.querySelector('#session-form textarea').focus({preventScroll:true}); document.querySelector('#main').scrollTop = 0`, nil)
	unblock()
	p.AwaitText("#session-feed", "Newest exploratory answer")
	feedAtBottom(p, "#session-feed")
	p.Await("the session composer retains focus and text", `document.activeElement === document.querySelector('#session-form textarea') && document.activeElement.value === 'Unsent exploratory draft'`)
	p.Click("#project-home")
	p.SelectSession(id)
	feedAtBottom(p, "#session-feed")
}

func TestBrowserDesignFeedScroll(t *testing.T) {
	ctx := context.Background()
	s, c, project, engine := newDesignEnforcementFixture(t, true)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		if strings.Contains(req.Prompt, "Newest design message") {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &agent.Result{ClaudeID: "design", ResultText: "Newest design answer"}, nil
		}
		return &agent.Result{ClaudeID: "design", ResultText: strings.Repeat(scrollReply, 45) + "<details><summary>Assumptions</summary>Keep the existing contract.</details>"}, nil
	}
	opened, err := c.OpenDesign(ctx, project)
	must(t, err)
	id := opened.Design.Workstream
	_, err = c.DesignMessage(ctx, id, "Seed design history")
	must(t, err)
	awaitDesignConversation(t, s, c, id, func(r ConversationResponse) bool { return len(r.Entries) == 2 && r.Entries[1].State == TurnDone })
	path := "/v1/design/" + string(id) + "/conversation"
	p := openFeedScrollPage(t, s, "#workstream="+string(id), path)
	p.Await("the design view opened before its conversation", `!document.querySelector('[data-view=design]').hidden`)
	releaseFeedRead(p, path)
	p.AwaitText("#design-feed", "Keep the existing contract.")
	feedAtBottom(p, "#design-feed")
	p.Click("#design-feed summary")
	scrollFeedUp(p)
	rereadFeed(p)
	feedScrollUnchanged(p)
	p.Await("the disclosure survives unchanged reads", `document.querySelector('#design-feed details').open`)
	p.Click("#project-home")
	p.SelectDesign(id)
	feedAtBottom(p, "#design-feed")
	p.TypeInto("#design-form textarea", "Newest design message")
	p.Eval(`feedHolds.add(`+quote(path)+`); document.querySelector('#design-form textarea').focus({preventScroll:true}); document.querySelector('#main').scrollTop = 0; document.querySelector('#design-form').requestSubmit()`, nil)
	p.AwaitText("#design-result", "Sent;")
	p.Await("the design reread is held", `typeof feedRelease[`+quote(path)+`] === 'function'`)
	assertFeedExcludes(t, p, "#design-feed", "Newest design message")
	releaseFeedRead(p, path)
	p.AwaitText("#design-feed", "Newest design message")
	feedAtBottom(p, "#design-feed")
	scrollFeedUp(p)
	rereadFeed(p)
	feedScrollUnchanged(p)
	p.TypeInto("#design-form textarea", "Unsent design draft")
	p.Eval(`document.querySelector('#design-form textarea').focus({preventScroll:true}); document.querySelector('#main').scrollTop = 0`, nil)
	unblock()
	p.AwaitText("#design-feed", "Newest design answer")
	feedAtBottom(p, "#design-feed")
	p.Await("the design composer retains focus and text", `document.activeElement === document.querySelector('#design-form textarea') && document.activeElement.value === 'Unsent design draft'`)
	p.Click("#project-home")
	p.Click(`[data-drilldown="` + string(id) + `"]`)
	feedAtBottom(p, "#design-feed")
}
