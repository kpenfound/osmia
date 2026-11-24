package browsertest

import (
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestBrowserPageShowsActiveWorkAndStaysCurrent(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	card := `[data-workstream="` + string(stream) + `"] `
	quietCard := `[data-workstream="` + string(quiet) + `"] `
	latest := card + `[data-kind=status][data-latest=true] `
	pause := `[data-pause="workstream:` + string(quiet) + `"] `

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.AwaitText(`[data-select="`+string(stream)+`"] [data-field=goal]`, "Ship resumable uploads.")
	p.AwaitText(`[data-select="`+string(quiet)+`"] [data-field=paused]`, "paused")
	p.SelectWorkstream(stream)
	for selector, want := range map[string]string{
		card + "[data-field=goal]":                           "Ship resumable uploads.",
		card + "[data-field=state]":                          "building",
		latest + "[data-field=note]":                         "A mason is building the upload unit.",
		latest + "[data-field=status-agents]":                "A mason builds upload.",
		card + "[data-field=workspaces]":                     "git workspaces",
		`#capacity [data-role=mason] [data-field=slots]`:     "1 / 1 slots",
		`#capacity [data-role=mason] [data-field=waiting]`:   string(stream) + " unit audit: every slot is taken",
		`#capacity [data-role=reviewer]`:                     "Nothing waits.",
		`#capacity [data-role=architect] [data-field=slots]`: "0 active (unbounded; 1 per workstream)",
		`#meter-total`:                "/",
		pause + "[data-field=scope]":  "Workstream " + string(quiet),
		pause + "[data-field=reason]": "The owner is travelling",
		pause + "[data-field=source]": "the owner",
		`#pause-count`:                "1",
	} {
		p.AwaitText(selector, want)
	}
	var cells []string
	p.Eval(`[...document.querySelectorAll(`+quote(card+"[data-field=units] [data-unit-state]")+`)].map((c) => c.dataset.unitState + '=' + c.getAttribute('aria-label'))`, &cells)
	if want := "merged=Unit resume: merged,implementing=Unit upload: implementing,ready=Unit audit: ready,planned=Unit index: planned"; strings.Join(cells, ",") != want {
		t.Fatalf("the nest tube cells are %q, want %q", cells, want)
	}
	p.Await("the mason meter full", `document.querySelector('[data-meter=mason]').dataset.full === 'true'`)
	p.Await("only the latest status and no machine event in Conversation", `document.querySelectorAll('#workstreams [data-kind=status]').length === 1 &&
		!document.querySelector('#workstreams [data-kind=session], #workstreams [data-kind=transition], #workstreams [data-kind=unit_checks]')`)
	p.OpenActivity()
	for selector, want := range map[string]string{
		card + `[data-kind=transition][data-unit=upload]`:                  "Unit upload: implementing",
		card + `[data-kind=session][data-role=mason] [data-field=session]`: "mason on upload default running",
	} {
		p.AwaitText(selector, want)
	}
	p.OpenConversation()
	var shown bool
	p.Eval(`document.querySelector(`+quote(card+"[data-field=attention]")+`) !== null || !document.getElementById('problems').hidden`, &shown)
	if shown {
		t.Fatal("the page shows an attention or a problem the views do not hold")
	}
	noNull := func(c string) {
		t.Helper()
		var text string
		p.Eval(`[...document.querySelectorAll(`+quote(strings.TrimSpace(c))+`)].map((e) => e.textContent).join(' ')`, &text)
		if strings.Contains(text, "null") {
			t.Fatalf("the workstream %s shows null: %q", c, text)
		}
	}
	noNull(card)
	p.SelectWorkstream(quiet)
	p.AwaitText(quietCard+"[data-field=goal]", "No status yet")
	p.AwaitText(quietCard+"[data-field=feed]", "Nothing here yet.")
	noNull(quietCard)
	p.SelectWorkstream(stream)

	p.Eval(`window.notReloaded = true`, nil)
	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads with dedupe.", Attention: "Rule on the upload API.", Note: "Upload waits for a ruling.", Agents: []string{"A mason builds upload."}})
	p.AwaitText(card+"[data-field=goal]", "Ship resumable uploads with dedupe.")
	p.AwaitText(`[data-select="`+string(stream)+`"] [data-field=goal]`, "Ship resumable uploads with dedupe.")
	p.AwaitText(card+"[data-field=attention]", "Rule on the upload API.")
	p.AwaitText(latest+"[data-field=note]", "Upload waits for a ruling.")
	p.Await("only the latest status in Conversation", `document.querySelectorAll('#workstreams [data-kind=status]').length === 1`)
	p.OpenActivity()
	p.AwaitText(card+`[data-kind=status][data-latest=false] [data-field=note]`, "A mason is building the upload unit.")
	p.AwaitText(card+`[data-kind=status][data-latest=true] [data-field=note]`, "Upload waits for a ruling.")
	p.OpenConversation()
	p.Await("the same document", `window.notReloaded === true`)

	type layout struct {
		Overflow, Fits, SideBySide, ListShown, Covers bool
	}
	measure := `(() => {
		const width = document.documentElement.clientWidth;
		const list = document.getElementById('sidebar').getBoundingClientRect();
		const main = document.getElementById('main').getBoundingClientRect();
		const shown = [...document.querySelectorAll('#workstream-head, #workstreams article')].map((e) => e.getBoundingClientRect());
		return {
			overflow: document.documentElement.scrollWidth > width,
			fits: shown.length === 2 && shown.every((r) => r.left >= 0 && r.right <= width),
			sideBySide: list.width > 0 && list.right <= main.left,
			listShown: getComputedStyle(document.getElementById('sidebar')).display !== 'none',
			covers: list.left === 0 && list.top === 0 && Math.round(list.width) === width && Math.round(list.height) === document.documentElement.clientHeight,
		};
	})()`
	var laptop, phone, picking layout
	p.Eval(measure, &laptop)
	if laptop.Overflow || !laptop.Fits || !laptop.SideBySide || !laptop.ListShown {
		t.Fatalf("layout at laptop width %+v", laptop)
	}
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile))
	p.Eval(measure, &phone)
	if phone.Overflow || !phone.Fits || phone.ListShown {
		t.Fatalf("layout at phone width %+v", phone)
	}
	p.Click("#picker-toggle")
	p.Await("the open list", `document.body.dataset.picker === 'open'`)
	p.Eval(measure, &picking)
	if picking.Overflow || !picking.ListShown || !picking.Covers {
		t.Fatalf("the open list at phone width %+v", picking)
	}
	p.Click("#picker-close")
	p.Await("the closed list", `document.body.dataset.picker === 'closed' && getComputedStyle(document.getElementById('sidebar')).display === 'none'`)
	p.SelectWorkstream(quiet)
	p.SelectWorkstream(stream)
	p.Await("the same document", `window.notReloaded === true`)

	link := newLinkProxy(t, f.s.WebAddr())
	p.Run(chromedp.Navigate("http://" + link.Listener.Addr().String() + "/"))
	p.Await("the live connection through the link", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(stream)
	p.AwaitText(card+"[data-field=goal]", "Ship resumable uploads with dedupe.")
	p.Eval(`window.notReloaded = true`, nil)
	link.Cut()
	p.Await("the lost connection", `document.body.dataset.connection === 'lost'`)
	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads with dedupe.", Note: "The ruling came; upload continues.", Agents: []string{"A mason builds upload."}})
	mutation(t, f.c, "DELETE", "pause", ClearPauseRequest{Scope: "workstream", Project: project, Workstream: quiet})
	var stale string
	p.Eval(textOf(latest+"[data-field=note]"), &stale)
	if stale != "Upload waits for a ruling." {
		t.Fatalf("the page changed while its stream was lost: %q", stale)
	}
	link.Restore()
	p.Await("the live connection after the loss", `document.body.dataset.connection === 'live'`)
	p.AwaitText(latest+"[data-field=note]", "The ruling came; upload continues.")
	p.Await("no attention", `document.querySelector(`+quote(card+"[data-field=attention]")+`) === null`)
	p.AwaitText("#pause-list", "Nothing is paused.")
	p.Await("no pause count", `document.getElementById('pause-count').textContent === ''`)
	p.Await("the same document", `window.notReloaded === true`)

	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: stream}, Mode: "soft", Reason: "Hold the uploads", Source: runtime.PauseOwner})
	livePause := `[data-pause="workstream:` + string(stream) + `"] `
	p.AwaitText(livePause+"[data-field=reason]", "Hold the uploads")
	p.AwaitText(livePause+"[data-field=source]", "the owner")
	p.AwaitText(`[data-select="`+string(stream)+`"] [data-field=paused]`, "paused")
	p.Await("the same document", `window.notReloaded === true`)

	var views []string
	for _, path := range p.Paths() {
		switch {
		case path == "/" || path == "/app.js" || path == "/style.css" || path == "/favicon.svg" || strings.HasPrefix(path, "/fonts/") && strings.HasSuffix(path, ".woff2"):
		case strings.HasPrefix(path, Prefix+"/"):
			views = append(views, path)
		default:
			t.Errorf("the page requested %s", path)
		}
	}
	for _, want := range []string{Prefix + "/status", Prefix + "/runtime", Prefix + "/events"} {
		if !slices.Contains(views, want) {
			t.Errorf("the page never requested %s: %v", want, views)
		}
	}
}

func TestBrowserSelectionMarksActivityWithoutReordering(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	streamRow := `[data-select="` + string(stream) + `"] `

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Await("both workstreams listed", `document.querySelectorAll('#workstream-list [data-select]').length === 2`)
	p.Await("the feeds read", `document.querySelector('.feed [data-state=loading]') === null`)
	p.Await("nothing marked", `document.querySelector('[data-field=unread]') === null`)

	order := p.SidebarIDs()
	sameOrder := func(what string) {
		t.Helper()
		if got := p.SidebarIDs(); !slices.Equal(got, order) {
			t.Fatalf("%s changed the sidebar order: %v, want %v", what, got, order)
		}
	}

	p.SelectWorkstream(quiet)
	sameOrder("selecting quiet")
	p.SelectWorkstream(stream)
	sameOrder("selecting stream")
	p.SelectWorkstream(quiet)
	sameOrder("selecting quiet again")

	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads.", Note: "Upload landed.", Agents: []string{"A mason builds upload."}})
	p.Await("the changed workstream marked", `document.querySelector(`+quote(streamRow+"[data-field=unread]")+`) !== null`)
	p.Await("the shown workstream unmarked", `document.querySelector(`+quote(`[data-select="`+string(quiet)+`"] [data-field=unread]`)+`) === null`)
	sameOrder("a status change on the workstream not shown")

	p.SelectWorkstream(stream)
	p.Await("the mark cleared", `document.querySelector('[data-field=unread]') === null`)
	sameOrder("clearing the mark")

	p.Run(chromedp.Reload())
	p.Await("the live connection after the reload", `document.body.dataset.connection === 'live'`)
	p.Await("the reloaded workstream shown", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(stream)))
	p.Await("nothing marked after the reload", `document.querySelector('[data-field=unread]') === null`)
	sameOrder("the reload")
}
