package browsertest

import (
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// The page, opened through the web listener, lists the workstreams beside
// the one it shows, and shows its goal, workspace backend and units by state,
// and a feed of its statuses, sessions and unit state changes, with the
// capacity and the pauses from the /v1 views; it follows a status change
// without a reload, at phone and laptop widths, and after its event stream is
// lost it reconnects and reads again what changed meanwhile.
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
		card + "[data-field=goal]":                                         "Ship resumable uploads.",
		card + "[data-field=state]":                                        "building",
		latest + "[data-field=note]":                                       "A mason is building the upload unit.",
		latest + "[data-field=status-agents]":                              "A mason builds upload.",
		card + "[data-field=workspaces]":                                   "git workspaces",
		card + `[data-unit-state=merged]`:                                  "merged 1",
		card + `[data-unit-state=implementing]`:                            "implementing 1",
		card + `[data-unit-state=ready]`:                                   "ready 1",
		card + `[data-unit-state=planned]`:                                 "planned 1",
		card + `[data-kind=transition][data-unit=upload]`:                  "Unit upload: implementing",
		card + `[data-kind=session][data-role=mason] [data-field=session]`: "mason on upload · default · running",
		`#capacity [data-role=mason] [data-field=slots]`:                   "1 / 1 slots",
		`#capacity [data-role=mason] [data-field=waiting]`:                 string(stream) + " unit audit: every slot is taken",
		`#capacity [data-role=reviewer]`:                                   "Nothing waits.",
		`#capacity [data-role=architect] [data-field=slots]`:               "0 active (1 per workstream)",
		`#capacity [data-role=librarian] [data-field=slots]`:               "0 active (1 per workstream)",
		`#meter-total`:                "/",
		pause + "[data-field=scope]":  "Workstream " + string(quiet),
		pause + "[data-field=reason]": "The owner is travelling",
		pause + "[data-field=source]": "the owner",
		`#pause-count`:                "1",
	} {
		p.AwaitText(selector, want)
	}
	p.Await("the mason meter full", `document.querySelector('[data-meter=mason]').dataset.full === 'true'`)
	var shown bool
	p.Eval(`document.querySelector(`+quote(card+"[data-field=attention]")+`) !== null || !document.getElementById('problems').hidden`, &shown)
	if shown {
		t.Fatal("the page shows an attention or a problem the views do not hold")
	}
	// A workstream without an attention item, and one without a status,
	// show no "null" text.
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
	p.AwaitText(quietCard+"[data-field=feed]", "Nothing has happened yet.")
	noNull(quietCard)
	p.SelectWorkstream(stream)

	// A status change appears without a reload.
	p.Eval(`window.notReloaded = true`, nil)
	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads with dedupe.", Attention: "Rule on the upload API.", Note: "Upload waits for a ruling.", Agents: []string{"A mason builds upload."}})
	p.AwaitText(card+"[data-field=goal]", "Ship resumable uploads with dedupe.")
	p.AwaitText(`[data-select="`+string(stream)+`"] [data-field=goal]`, "Ship resumable uploads with dedupe.")
	p.AwaitText(card+"[data-field=attention]", "Rule on the upload API.")
	p.AwaitText(latest+"[data-field=note]", "Upload waits for a ruling.")
	p.AwaitText(card+`[data-kind=status][data-latest=false] [data-field=note]`, "A mason is building the upload unit.")
	p.Await("the same document", `window.notReloaded === true`)

	// Laptop widths show the list beside the workstream. Phone widths
	// collapse the list behind a button; opened, it covers the page until a
	// workstream is picked. Neither scrolls sideways.
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

	// The stream is lost; what changes meanwhile is read once the page
	// reconnects.
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

	// A pause set while the stream is live appears without a reload.
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: stream}, Mode: "soft", Reason: "Hold the uploads", Source: runtime.PauseOwner})
	livePause := `[data-pause="workstream:` + string(stream) + `"] `
	p.AwaitText(livePause+"[data-field=reason]", "Hold the uploads")
	p.AwaitText(livePause+"[data-field=source]", "the owner")
	p.AwaitText(`[data-select="`+string(stream)+`"] [data-field=paused]`, "paused")
	p.Await("the same document", `window.notReloaded === true`)

	// Everything the page showed came from the page's own files and /v1.
	var views []string
	for _, path := range p.Paths() {
		switch {
		case path == "/" || path == "/app.js" || path == "/style.css" || path == "/favicon.svg":
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

// A workstream that changes while the owner looks at another is marked, and
// looking at it clears the mark. What the page first reads counts as seen,
// and the selection and what was seen survive a reload. Selecting a
// workstream, a status change on one not shown and a reload never change the
// sidebar's order, which the server already sorts by last activity.
// serial: asserts markSeen's unread tracking, which app.js gates on
// document.visibilityState === 'visible' (internal/web/app.js); openBrowser's
// shared browser window has one visible tab at a time, and a concurrent
// test's chromedp.BringToFront() races this test's visibility.
func TestBrowserSelectionMarksActivityWithoutReordering(t *testing.T) {
	f := newPageFixture(t)
	p := openBrowser(t)
	streamRow := `[data-select="` + string(stream) + `"] `

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Await("both workstreams listed", `document.querySelectorAll('#workstream-list [data-select]').length === 2`)
	p.Await("the feeds read", `document.querySelector('[data-field=feed] .meta')?.textContent !== 'Reading the feed…'`)
	// markSeen runs synchronously inside app.js's render(), so there is no
	// transient window to wait out: this await is itself the readiness check.
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

	// A status change on the workstream not shown marks it, without moving it.
	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads.", Note: "Upload landed.", Agents: []string{"A mason builds upload."}})
	p.Await("the changed workstream marked", `document.querySelector(`+quote(streamRow+"[data-field=unread]")+`) !== null`)
	p.Await("the shown workstream unmarked", `document.querySelector(`+quote(`[data-select="`+string(quiet)+`"] [data-field=unread]`)+`) === null`)
	sameOrder("a status change on the workstream not shown")

	// Looking at it clears the mark, without moving it.
	p.SelectWorkstream(stream)
	p.Await("the mark cleared", `document.querySelector('[data-field=unread]') === null`)
	sameOrder("clearing the mark")

	p.Run(chromedp.Reload())
	p.Await("the live connection after the reload", `document.body.dataset.connection === 'live'`)
	p.Await("the reloaded workstream shown", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(stream)))
	p.Await("nothing marked after the reload", `document.querySelector('[data-field=unread]') === null`)
	sameOrder("the reload")
}
