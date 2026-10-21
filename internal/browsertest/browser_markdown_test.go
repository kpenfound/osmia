package browsertest

import (
	"context"
	"strconv"
	"strings"
	"testing"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const browserMarkdown = "# Rendered heading\n\nA **bold** and *italic* reply with `inline` code.\nSecond line.\n\n```go\n" +
	"a_very_long_code_line_" + "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz" +
	"\n```\n\n- First bullet\n  - Nested bullet\n    1. Nested number\n- Second bullet\n\n1. First number\n2. Second number\n\n> A quoted **statement**\n\n" +
	"[secure](https://example.invalid/path) [web](http://example.invalid) [mail](mailto:owner@example.invalid)\n\n" +
	"| Name | Description | Count |\n| :--- | :---: | ---: |\n| Alpha | A wide description that should scroll within the table box at phone widths | 12 |\n| Beta | Another description | 3 |"

// watchMarkdownEvents observes the page's real EventSource, with periodic
// refresh disabled so an arriving reply must be delivered by the live path.
func watchMarkdownEvents(p *page) {
	p.T.Helper()
	p.Run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(`
    window.feedEvents = []; window.sameFeedDocument = true;
    window.setInterval = () => 0;
    const LiveEvents = window.EventSource;
    window.EventSource = class extends LiveEvents {
      constructor(...args) { super(...args);
        this.addEventListener('workstream', e => feedEvents.push(JSON.parse(e.data)));
      }
    };`).Do(ctx)
		return err
	}))
}

func assertMarkdown(p *page, selector string) string {
	p.T.Helper()
	p.Await("formatted Markdown in "+selector, `(() => {
  const n = document.querySelector(`+quote(selector)+`);
  return n && n.querySelector('h1')?.textContent === 'Rendered heading' &&
    n.querySelector('strong')?.textContent === 'bold' && n.querySelector('em')?.textContent === 'italic' &&
    n.querySelector('p code')?.textContent === 'inline' && n.querySelector('pre code')?.textContent.startsWith('a_very_long_code_line_') &&
    n.querySelectorAll('table thead th').length === 3 && n.querySelectorAll('table tbody tr').length === 2 &&
    [...n.querySelectorAll('table tbody tr')].every(r => r.cells.length === 3) &&
    n.querySelector('th.align-center') && n.querySelector('td.align-right') &&
    n.querySelector('ul ul ol li')?.textContent === 'Nested number' &&
    n.querySelector('blockquote strong')?.textContent === 'statement' && n.querySelectorAll('a').length === 3 &&
    !n.textContent.includes('| :---') && !n.textContent.includes('**bold**');
 })()`)
	var structure string
	p.Eval(`document.querySelector(`+quote(selector)+`).innerHTML`, &structure)
	return structure
}

func assertMarkdownPhone(p *page, selector string) {
	p.T.Helper()
	p.Await("contained table and code scrolling", `(() => {
  const n = document.querySelector(`+quote(selector)+`);
  return document.documentElement.scrollWidth <= 390 &&
    ['.markdown-table', 'pre'].every(s => {
      const box = n.querySelector(s);
      if (!box || box.scrollWidth <= box.clientWidth || getComputedStyle(box).overflowX !== 'auto') return false;
      box.scrollLeft = 30; return box.scrollLeft > 0;
    });
 })()`)
}

func TestBrowserAgentMarkdownAcrossConversations(t *testing.T) {
	ctx := context.Background()
	f := newPageFixture(t)
	p := openBrowser(t)
	watchMarkdownEvents(p)
	p.Run(chromedp.EmulateViewport(390, 844), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(quiet)
	const owner = "# Owner **literal**\n| no | formatting |"
	f.sendAndAnswer(t, p, quiet, owner, browserMarkdown)
	awaitFeedEvent(p, quiet)
	chief := `[data-workstream="` + string(quiet) + `"] [data-kind=response] [data-field=text] > .markdown`
	expected := assertMarkdown(p, chief)
	assertMarkdownPhone(p, chief)
	p.Await("plain owner message", `document.querySelector('[data-kind=message] [data-field=text]').textContent === `+strconv.Quote(owner)+` && !document.querySelector('[data-kind=message] [data-field=text] h1')`)
	conversation, err := f.c.Conversation(ctx, quiet)
	must(t, err)
	if conversation.Entries[1].Text != browserMarkdown {
		t.Fatal("conversation did not retain raw Markdown")
	}
	feed := readFeed(t, f.c, quiet)
	raw := false
	for _, entry := range feed.Entries {
		if entry.Kind == "response" && entry.Text == browserMarkdown {
			raw = true
		}
	}
	if !raw {
		t.Fatal("feed did not retain raw Markdown")
	}

	// The exploratory reply travels through the real session runtime; its
	// owner echo is created by the composer rather than an API-only send.
	s, c, project, engine, _ := sessionWebFixture(t)
	engine.Turn = func(context.Context, agent.Request) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "markdown-session", ResultText: browserMarkdown}, nil
	}
	opened, err := c.OpenSession(ctx, SessionOpenRequest{Project: project, Profile: "default"})
	must(t, err)
	ps := openBrowser(t)
	watchMarkdownEvents(ps)
	ps.Run(chromedp.EmulateViewport(390, 844), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	ps.Await("live connection", `document.body.dataset.connection === 'live'`)
	ps.SelectSession(opened.Session.Workstream)
	ps.TypeInto("#session-form textarea", owner)
	ps.Click("#session-form [type=submit]")
	awaitFeedEvent(ps, opened.Session.Workstream)
	session := `#session-feed [data-kind=response] [data-field=text] > .markdown`
	if got := assertMarkdown(ps, session); got != expected {
		t.Fatalf("session structure differs from chief: %s", got)
	}
	assertMarkdownPhone(ps, session)
	ps.Await("session owner text as typed", `document.querySelector('#session-feed [data-kind=message] [data-field=text]').textContent === `+strconv.Quote(owner))

	ds, dc, dp, de := newDesignEnforcementFixture(t, true)
	const assumption = "## Assumed\n\n**Keep** the contract.\n\n| Choice | Reason |\n| --- | --- |\n| Stable | Compatibility |"
	const reply = browserMarkdown + "\n<details><summary>Assumptions</summary>" + assumption + "</details>\n\n**After** assumptions."
	de.Turns["*"] = func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "markdown-design", ResultText: reply}, nil
	}
	pd := openBrowser(t)
	watchMarkdownEvents(pd)
	pd.Run(chromedp.EmulateViewport(390, 844), chromedp.Navigate("http://"+ds.WebAddr()+"/"))
	pd.Await("live connection", `document.body.dataset.connection === 'live'`)
	pd.Click(`[data-project="` + string(dp) + `"] [data-project-action="design-add"]`)
	pd.Choose("#design-add-form [data-project-select]", string(dp))
	pd.Click("#design-add-form [type=submit]")
	pd.AwaitText("#design-add-form .result", "Opened design session")
	designs, err := dc.Designs(ctx, dp)
	must(t, err)
	id := designs.Designs[0].Workstream
	pd.TypeInto("#design-form textarea", owner)
	pd.Click("#design-form [type=submit]")
	pd.AwaitText("#design-result", "the design agent answers in its next turn")
	awaitFeedEvent(pd, id)
	design := `#design-feed [data-kind=response] [data-field=text] > .markdown`
	// The surrounding segment ends at the disclosure boundary, after the
	// same Markdown document and a trailing newline (no visible structure).
	got := assertMarkdown(pd, design)
	if strings.TrimSpace(got) != strings.TrimSpace(expected) {
		t.Fatalf("design structure differs from chief: %s", got)
	}
	assertMarkdownPhone(pd, design)
	pd.AwaitText("#design-feed details summary", "Assumptions (reply to override)")
	pd.Click("#design-feed details summary")
	pd.Await("rendered assumptions and surrounding text", `document.querySelector('#design-feed details').open &&
   document.querySelector('#design-feed details h2').textContent === 'Assumed' &&
   document.querySelector('#design-feed details table tbody td').textContent === 'Stable' &&
   [...document.querySelectorAll('#design-feed strong')].some(n => n.textContent === 'After')`)
	pd.Eval(`window.originalAssumptions = document.querySelector('#design-feed details').innerHTML; window.feedEvents.length = 0`, nil)
	pd.TypeInto("#design-form textarea", "A new owner message")
	pd.Click("#design-form [type=submit]")
	awaitFeedEvent(pd, id)
	pd.Await("new reply retains expanded original assumptions", `document.querySelectorAll('#design-feed [data-kind=response]').length === 2 &&
   document.querySelector('#design-feed details').open && document.querySelector('#design-feed details').innerHTML === window.originalAssumptions`)
	recorded, err := dc.DesignConversation(ctx, id)
	must(t, err)
	if recorded.Entries[1].Text != reply {
		t.Fatal("design API did not retain raw text")
	}
}

func TestBrowserMarkdownStatusAndReportsStayExpanded(t *testing.T) {
	f := newPageFixture(t)
	f.status(t, trace.StatusContent{Goal: "Markdown reports", Note: browserMarkdown, Attention: browserMarkdown, Agents: []string{}})
	f.answerChief(t, stream, f.chief.Turn, browserMarkdown)
	p := openBrowser(t)
	watchMarkdownEvents(p)
	p.Run(chromedp.EmulateViewport(390, 844), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(stream)
	card := `[data-workstream="` + string(stream) + `"] `
	assertMarkdown(p, card+`[data-latest=true] [data-field=note] > .markdown`)
	assertMarkdown(p, card+`[data-latest=true] [data-field=attention] > .markdown`)
	p.Click(card + `[data-kind=session][data-role=chief_of_staff] summary`)
	report := card + `[data-kind=session][data-role=chief_of_staff] [data-field=summary] > .markdown`
	assertMarkdown(p, report)
	assertMarkdownPhone(p, report)
	p.Eval(`window.feedEvents.length = 0`, nil)
	// A new reply forces replacement of the feed entries while the report
	// remains open. The existing anchor still addresses this agent turn.
	f.sendAndAnswer(t, p, stream, "Refresh while I read the report.", "A new reply.")
	awaitFeedEvent(p, stream)
	p.Await("report stays open after a feed update", `document.querySelector(`+quote(card+`[data-kind=session][data-role=chief_of_staff] details`)+`).open &&
	   [...document.querySelectorAll(`+quote(card+`[data-kind=response]`)+`)].some(n => n.textContent.includes('A new reply.'))`)
	assertMarkdown(p, report)
	p.Await("session anchor", `document.getElementById(`+quote("feed-turn-"+f.chief.Turn)+`)?.querySelector('details').open`)
}
