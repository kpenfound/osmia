package service

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// browserTimeout bounds each wait for the page to show something.
const browserTimeout = 30 * time.Second

// page drives one headless Chromium tab and records every URL it requests.
type page struct {
	t         *testing.T
	ctx       context.Context
	mu        sync.Mutex
	requested []string
}

// sharedBrowserOnce gates sharedBrowser so Chromium starts at most once per
// package run. Every browser test gets its own tab on the browser it
// starts, through sharedBrowser; TestMain shuts the browser down once every
// test has run.
var (
	sharedBrowserOnce sync.Once
	sharedBrowserCtx  context.Context
	sharedBrowserDone context.CancelFunc
	sharedBrowserErr  error
)

// sharedBrowser starts the Chromium binary named by binary on its first
// call, and returns the context of the long-lived tab that run, for every
// later call in the package to derive its own tab from with
// chromedp.NewContext.
func sharedBrowser(t *testing.T, binary string) context.Context {
	t.Helper()
	sharedBrowserOnce.Do(func() {
		options := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(binary), chromedp.NoSandbox,
			chromedp.Flag("disable-dev-shm-usage", true))
		allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(), options...)
		ctx, cancel := chromedp.NewContext(allocator)
		sharedBrowserCtx = ctx
		sharedBrowserDone = func() {
			cancel()
			cancelAllocator()
		}
		// The first run starts the browser itself, on the long-lived
		// context, before any test's own tab or per-action timeout.
		sharedBrowserErr = chromedp.Run(ctx, network.Enable())
	})
	if sharedBrowserErr != nil {
		t.Fatalf("starting the shared browser: %v", sharedBrowserErr)
	}
	return sharedBrowserCtx
}

// TestMain applies this package's shuffle and timeout defaults
// (applyServiceTestDefaults), runs the tests, and shuts down the browser
// sharedBrowser started, if any, once every test in the package has run.
func TestMain(m *testing.M) {
	if !flag.Parsed() {
		flag.Parse()
	}
	applyServiceTestDefaults(flag.CommandLine, serviceTestTimeoutCap)
	code := m.Run()
	if sharedBrowserDone != nil {
		sharedBrowserDone()
	}
	os.Exit(code)
}

// openBrowser gives the test its own tab, on the Chromium binary
// OSMIA_BROWSER names, starting it on the package's first call, or skips the
// test without one. The browser Dagger check sets it. Every tab lives in the
// one browser context and window sharedBrowser's long-lived tab opened; a
// second browser context's first target has no window to open in under
// headless Chromium and Target.createTarget fails with "no browser is open",
// and chromedp's own browser-context creation offers no way to request one
// (target.CreateTargetParams.NewWindow, which would, is also documented as
// unsupported by headless shell). A test's tab doesn't need its own browser
// context anyway: each fixture's web listener binds its own loopback port, so
// localStorage, sessionStorage and cache are already isolated per test by
// origin. Bringing the tab to the front as soon as it opens keeps it the
// visible, focused tab regardless of which other tabs (including
// sharedBrowser's own) still exist in that window, which some pages need:
// app.js only polls for configuration drift, and only marks a workstream
// seen, while document.visibilityState is "visible". t.Cleanup closes the
// tab before the fixture that ran it tears down the service and its
// workspace, since cleanups run in last-registered-first-run order and this
// one is registered after the fixture's.
func openBrowser(t *testing.T) *page {
	t.Helper()
	binary := os.Getenv("OSMIA_BROWSER")
	if binary == "" {
		t.Skip("OSMIA_BROWSER names no Chromium binary; dagger check runs the browser tests")
	}
	root := sharedBrowser(t, binary)
	ctx, cancel := chromedp.NewContext(root)
	t.Cleanup(cancel)
	p := &page{t: t, ctx: ctx}
	chromedp.ListenTarget(ctx, func(event any) {
		if e, ok := event.(*network.EventRequestWillBeSent); ok {
			p.mu.Lock()
			p.requested = append(p.requested, e.Request.URL)
			p.mu.Unlock()
		}
	})
	// The first run opens this test's own tab, on its own long-lived
	// context, before any later action's per-run timeout, and brings it to
	// the front before anything else can observe its visibility.
	must(t, chromedp.Run(ctx, network.Enable(), cdppage.BringToFront()))
	return p
}

// run runs the actions, and fails with the page's text when one fails.
func (p *page) run(actions ...chromedp.Action) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(p.ctx, browserTimeout)
	defer cancel()
	if err := chromedp.Run(ctx, actions...); err != nil {
		var text string
		shown, cancel := context.WithTimeout(p.ctx, 5*time.Second)
		defer cancel()
		chromedp.Run(shown, chromedp.Evaluate("document.body?.innerText ?? ''", &text))
		p.t.Fatalf("%v; the page shows:\n%s", err, text)
	}
}

func (p *page) eval(expression string, out any) {
	p.t.Helper()
	p.run(chromedp.Evaluate(expression, out))
}

// await waits until the expression is true, and fails with the page's text
// when it stays false.
func (p *page) await(what, expression string) {
	p.t.Helper()
	deadline := time.Now().Add(browserTimeout)
	for {
		var ok bool
		p.eval("!!("+expression+")", &ok)
		if ok {
			return
		}
		if time.Now().After(deadline) {
			var text, connection string
			p.eval("document.body?.innerText ?? ''", &text)
			p.eval("document.body?.dataset.connection ?? ''", &connection)
			p.t.Fatalf("the page never showed %s (connection %s):\n%s", what, connection, text)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// quote returns s as a JavaScript string literal.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// textOf is an expression for the text of the first element selector
// matches, or the empty string without one.
func textOf(selector string) string {
	return "(document.querySelector(" + quote(selector) + ")?.textContent ?? '')"
}

// awaitText waits until the element's text contains want.
func (p *page) awaitText(selector, want string) {
	p.t.Helper()
	p.await(fmt.Sprintf("%q in %s", want, selector), textOf(selector)+".includes("+quote(want)+")")
}

// click clicks the first element selector matches, once it is scrolled to
// the middle of the page, clear of the sticky heading and message field.
func (p *page) click(selector string) {
	p.t.Helper()
	p.eval(`document.querySelector(`+quote(selector)+`)?.scrollIntoView({block: 'center'})`, nil)
	p.run(chromedp.Click(selector, chromedp.ByQuery))
}

// typeInto types text into the first field selector matches.
func (p *page) typeInto(selector, text string) {
	p.t.Helper()
	p.run(chromedp.SendKeys(selector, text, chromedp.ByQuery))
}

// choose selects value in the first select selector matches.
func (p *page) choose(selector, value string) {
	p.t.Helper()
	p.run(chromedp.SetValue(selector, value, chromedp.ByQuery))
}

// selectWorkstream picks a workstream from the list the way the owner does,
// opening the list first where it is collapsed, and waits until the main
// area shows it.
func (p *page) selectWorkstream(id config.WorkstreamID) {
	p.t.Helper()
	row := `[data-select="` + string(id) + `"]`
	p.await("the workstream "+string(id)+" in the list", `document.querySelector(`+quote(row)+`) !== null`)
	var collapsed bool
	p.eval(`getComputedStyle(document.getElementById('sidebar')).display === 'none'`, &collapsed)
	if collapsed {
		p.click("#picker-toggle")
	}
	p.click(row)
	p.await("the workstream "+string(id)+" shown", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(id))+
		` && document.body.dataset.picker === 'closed'`)
}

// sidebarIDs reads the workstream IDs the sidebar's list of work shows, top
// to bottom.
func (p *page) sidebarIDs() []string {
	p.t.Helper()
	var joined string
	p.eval(`[...document.querySelectorAll('#workstream-list [data-select]')].map((e) => e.dataset.select).join(',')`, &joined)
	if joined == "" {
		return nil
	}
	return strings.Split(joined, ",")
}

// awaitSidebarOrder waits until the sidebar's list of work shows exactly
// want, top to bottom.
func (p *page) awaitSidebarOrder(want ...config.WorkstreamID) {
	p.t.Helper()
	ids := make([]string, len(want))
	for i, w := range want {
		ids[i] = string(w)
	}
	joined := strings.Join(ids, ",")
	expr := `[...document.querySelectorAll('#workstream-list [data-select]')].map((e) => e.dataset.select).join(',') === ` + quote(joined)
	p.await("the sidebar ordered "+joined, expr)
}

// closePopovers closes the header's open popovers and menus.
func (p *page) closePopovers() {
	p.t.Helper()
	p.eval(`document.querySelectorAll(':popover-open').forEach((e) => e.hidePopover())`, nil)
}

// openView opens one of the views the settings menu lists.
func (p *page) openView(name string) {
	p.t.Helper()
	p.closePopovers()
	p.click("#settings-button")
	p.click(`#settings-menu [data-open="` + name + `"]`)
	p.await("the "+name+" view", `!document.querySelector('.view[data-view="`+name+`"]').hidden`)
}

// openPauses opens the header's pause popover unless it is open.
func (p *page) openPauses() {
	p.t.Helper()
	var open bool
	p.eval(`document.getElementById('pause-popover').matches(':popover-open')`, &open)
	if !open {
		p.click("#pause-button")
		p.await("the pauses", `document.getElementById('pause-popover').matches(':popover-open')`)
	}
}

// paths returns the paths of every URL the page requested.
func (p *page) paths() []string {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, raw := range p.requested {
		u, err := url.Parse(raw)
		must(p.t, err)
		out = append(out, u.Path)
	}
	return out
}

// linkProxy forwards loopback connections to the web listener. cut ends
// every forwarded connection and refuses new ones until restore.
type linkProxy struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	down     bool
	conns    map[net.Conn]struct{}
}

func newLinkProxy(t *testing.T, target string) *linkProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	p := &linkProxy{listener: l, target: target, conns: map[net.Conn]struct{}{}}
	go func() {
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			go p.forward(client)
		}
	}()
	t.Cleanup(func() {
		l.Close()
		p.cut()
	})
	return p
}

func (p *linkProxy) forward(client net.Conn) {
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		client.Close()
		return
	}
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		client.Close()
		server.Close()
		return
	}
	p.conns[client], p.conns[server] = struct{}{}, struct{}{}
	p.mu.Unlock()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		io.Copy(dst, src)
		dst.Close()
		src.Close()
		done <- struct{}{}
	}
	go pipe(server, client)
	go pipe(client, server)
	<-done
	<-done
	p.mu.Lock()
	delete(p.conns, client)
	delete(p.conns, server)
	p.mu.Unlock()
}

func (p *linkProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	for c := range p.conns {
		c.Close()
	}
}

func (p *linkProxy) restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = false
}

// pageFixture is a started service with a web listener whose trace holds
// stream building four units, with a running mason, a mason waiting for the
// one mason slot and a chief of staff in a turn that writes the status, and
// quiet, paused by the owner, with no status.
type pageFixture struct {
	s     *Service
	c     *Client
	chief coreadapter.Scope
}

func newPageFixture(t *testing.T) *pageFixture {
	t.Helper()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "pg-")
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("[capacity]\nmasons = 1\n[listen]\nweb = \"127.0.0.1:0\"\n")
	must(t, err)
	must(t, configFile.Close())
	// The loop passes only when the workflow changes.
	opts.Reconciliation.Ticks = make(chan time.Time)
	at := time.Now().UTC()
	header := func(schema, id string, revision int) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: revision, Project: project, Workstream: stream, At: at, Actor: ownerActor, Cause: "planted"}
	}

	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	units := []struct{ id, state string }{{"resume", UnitMerged}, {"upload", UnitImplementing}, {"audit", UnitReady}, {"index", UnitPlanned}}
	var graph strings.Builder
	graph.WriteString(`{"version": 1, "units": [`)
	for i, u := range units {
		if i > 0 {
			graph.WriteString(", ")
		}
		fmt.Fprintf(&graph, `{"id": %q, "title": "Build %s", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestWork"}}], "depends_on": [], "footprint": []}`, u.id, u.id)
	}
	graph.WriteString("]}\n")
	_, err = plan.Parse([]byte(graph.String()))
	must(t, err)
	sealed, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash(validSpec),
		Base: seal.Base{Remote: "upstream", Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567"}, Branch: "osmia/" + string(stream), Workspace: "/planted"})
	must(t, err)
	must(t, repo.RecordDocuments(ctx, []trace.Document{
		{Header: header("osmia.trace.document", plan.PlanDocument, 1), Path: plan.PlanPath, Content: graph.String()},
		{Header: header("osmia.trace.document", seal.DocumentID, 1), Path: seal.Path, Content: string(sealed)},
	}))
	_, err = repo.SetFeatureState(ctx, header("osmia.trace.transition", "planted-building", 1), BuildingState, "planted")
	must(t, err)
	for _, u := range units {
		subject := trace.UnitSubject(u.id)
		state, err := repo.Workflow(stream, subject)
		must(t, err)
		h := header("osmia.trace.transition", subject+"-"+u.state, 1)
		h.Unit = u.id
		_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: u.state, Reason: "planted"}})
		must(t, err)
	}
	must(t, repo.Close())

	s, c := start(t, opts)
	f := &pageFixture{s: s, c: c}
	// The service abandons claimed chief-of-staff turns when it starts, so
	// the turns are claimed once it runs.
	live := s.sole().repository
	_, err = live.EnsureChiefOfStaff(ctx, stream, at, serviceActor)
	must(t, err)
	f.chief = claimTurn(t, live, t.TempDir(), stream, trace.ChiefOfStaff, trace.ChiefOfStaff, "status", at)
	queueRoleTurn(t, live, stream, "agent_mason_upload", masonRole, "upload", at)
	_, err = live.ClaimTurn(ctx, stream, "agent_mason_upload", "token_upload", t.TempDir(), at)
	must(t, err)
	queueRoleTurn(t, live, stream, "agent_mason_audit", masonRole, "audit", at)
	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads.", Note: "A mason is building the upload unit.", Agents: []string{"A mason builds upload."}})
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: quiet}, Mode: "soft", Reason: "The owner is travelling", Source: runtime.PauseOwner})
	return f
}

// status writes the next status revision through the chief of staff's turn.
func (f *pageFixture) status(t *testing.T, content trace.StatusContent) {
	t.Helper()
	_, err := f.s.sole().repository.SetStatus(context.Background(), trace.ChiefOfStaff, f.chief, content, time.Now().UTC(), func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	must(t, err)
}

// The page, opened through the web listener, lists the workstreams beside
// the one it shows, and shows its goal, workspace backend and units by state,
// and a feed of its statuses, sessions and unit state changes, with the
// capacity and the pauses from the /v1 views; it follows a status change
// without a reload, at phone and laptop widths, and after its event stream is
// lost it reconnects and reads again what changed meanwhile.
func TestBrowserPageShowsActiveWorkAndStaysCurrent(t *testing.T) {
	f := newPageFixture(t)
	p := openBrowser(t)
	card := `[data-workstream="` + string(stream) + `"] `
	quietCard := `[data-workstream="` + string(quiet) + `"] `
	latest := card + `[data-kind=status][data-latest=true] `
	pause := `[data-pause="workstream:` + string(quiet) + `"] `

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.awaitText(`[data-select="`+string(stream)+`"] [data-field=goal]`, "Ship resumable uploads.")
	p.awaitText(`[data-select="`+string(quiet)+`"] [data-field=paused]`, "paused")
	p.selectWorkstream(stream)
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
		`#meter-total`:                                                     "/",
		pause + "[data-field=scope]":                                       "Workstream " + string(quiet),
		pause + "[data-field=reason]":                                      "The owner is travelling",
		pause + "[data-field=source]":                                      "the owner",
		`#pause-count`:                                                     "1",
	} {
		p.awaitText(selector, want)
	}
	p.await("the mason meter full", `document.querySelector('[data-meter=mason]').dataset.full === 'true'`)
	var shown bool
	p.eval(`document.querySelector(`+quote(card+"[data-field=attention]")+`) !== null || !document.getElementById('problems').hidden`, &shown)
	if shown {
		t.Fatal("the page shows an attention or a problem the views do not hold")
	}
	// A workstream without an attention item, and one without a status,
	// show no "null" text.
	noNull := func(c string) {
		t.Helper()
		var text string
		p.eval(`[...document.querySelectorAll(`+quote(strings.TrimSpace(c))+`)].map((e) => e.textContent).join(' ')`, &text)
		if strings.Contains(text, "null") {
			t.Fatalf("the workstream %s shows null: %q", c, text)
		}
	}
	noNull(card)
	p.selectWorkstream(quiet)
	p.awaitText(quietCard+"[data-field=goal]", "No status yet")
	p.awaitText(quietCard+"[data-field=feed]", "Nothing has happened yet.")
	noNull(quietCard)
	p.selectWorkstream(stream)

	// A status change appears without a reload.
	p.eval(`window.notReloaded = true`, nil)
	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads with dedupe.", Attention: "Rule on the upload API.", Note: "Upload waits for a ruling.", Agents: []string{"A mason builds upload."}})
	p.awaitText(card+"[data-field=goal]", "Ship resumable uploads with dedupe.")
	p.awaitText(`[data-select="`+string(stream)+`"] [data-field=goal]`, "Ship resumable uploads with dedupe.")
	p.awaitText(card+"[data-field=attention]", "Rule on the upload API.")
	p.awaitText(latest+"[data-field=note]", "Upload waits for a ruling.")
	p.awaitText(card+`[data-kind=status][data-latest=false] [data-field=note]`, "A mason is building the upload unit.")
	p.await("the same document", `window.notReloaded === true`)

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
	p.eval(measure, &laptop)
	if laptop.Overflow || !laptop.Fits || !laptop.SideBySide || !laptop.ListShown {
		t.Fatalf("layout at laptop width %+v", laptop)
	}
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile))
	p.eval(measure, &phone)
	if phone.Overflow || !phone.Fits || phone.ListShown {
		t.Fatalf("layout at phone width %+v", phone)
	}
	p.click("#picker-toggle")
	p.await("the open list", `document.body.dataset.picker === 'open'`)
	p.eval(measure, &picking)
	if picking.Overflow || !picking.ListShown || !picking.Covers {
		t.Fatalf("the open list at phone width %+v", picking)
	}
	p.click("#picker-close")
	p.await("the closed list", `document.body.dataset.picker === 'closed' && getComputedStyle(document.getElementById('sidebar')).display === 'none'`)
	p.selectWorkstream(quiet)
	p.selectWorkstream(stream)
	p.await("the same document", `window.notReloaded === true`)

	// The stream is lost; what changes meanwhile is read once the page
	// reconnects.
	link := newLinkProxy(t, f.s.WebAddr())
	p.run(chromedp.Navigate("http://" + link.listener.Addr().String() + "/"))
	p.await("the live connection through the link", `document.body.dataset.connection === 'live'`)
	p.selectWorkstream(stream)
	p.awaitText(card+"[data-field=goal]", "Ship resumable uploads with dedupe.")
	p.eval(`window.notReloaded = true`, nil)
	link.cut()
	p.await("the lost connection", `document.body.dataset.connection === 'lost'`)
	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads with dedupe.", Note: "The ruling came; upload continues.", Agents: []string{"A mason builds upload."}})
	mutation(t, f.c, "DELETE", "pause", ClearPauseRequest{Scope: "workstream", Project: project, Workstream: quiet})
	var stale string
	p.eval(textOf(latest+"[data-field=note]"), &stale)
	if stale != "Upload waits for a ruling." {
		t.Fatalf("the page changed while its stream was lost: %q", stale)
	}
	link.restore()
	p.await("the live connection after the loss", `document.body.dataset.connection === 'live'`)
	p.awaitText(latest+"[data-field=note]", "The ruling came; upload continues.")
	p.await("no attention", `document.querySelector(`+quote(card+"[data-field=attention]")+`) === null`)
	p.awaitText("#pause-list", "Nothing is paused.")
	p.await("no pause count", `document.getElementById('pause-count').textContent === ''`)
	p.await("the same document", `window.notReloaded === true`)

	// A pause set while the stream is live appears without a reload.
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: stream}, Mode: "soft", Reason: "Hold the uploads", Source: runtime.PauseOwner})
	livePause := `[data-pause="workstream:` + string(stream) + `"] `
	p.awaitText(livePause+"[data-field=reason]", "Hold the uploads")
	p.awaitText(livePause+"[data-field=source]", "the owner")
	p.awaitText(`[data-select="`+string(stream)+`"] [data-field=paused]`, "paused")
	p.await("the same document", `window.notReloaded === true`)

	// Everything the page showed came from the page's own files and /v1.
	var views []string
	for _, path := range p.paths() {
		switch {
		case path == "/" || path == "/app.js" || path == "/style.css":
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
func TestBrowserSelectionMarksActivityWithoutReordering(t *testing.T) {
	f := newPageFixture(t)
	p := openBrowser(t)
	streamRow := `[data-select="` + string(stream) + `"] `

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.await("both workstreams listed", `document.querySelectorAll('#workstream-list [data-select]').length === 2`)
	p.await("the feeds read", `document.querySelector('[data-field=feed] .meta')?.textContent !== 'Reading the feed…'`)
	settle()
	p.await("nothing marked", `document.querySelector('[data-field=unread]') === null`)

	order := p.sidebarIDs()
	sameOrder := func(what string) {
		t.Helper()
		if got := p.sidebarIDs(); !slices.Equal(got, order) {
			t.Fatalf("%s changed the sidebar order: %v, want %v", what, got, order)
		}
	}

	p.selectWorkstream(quiet)
	sameOrder("selecting quiet")
	p.selectWorkstream(stream)
	sameOrder("selecting stream")
	p.selectWorkstream(quiet)
	sameOrder("selecting quiet again")

	// A status change on the workstream not shown marks it, without moving it.
	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads.", Note: "Upload landed.", Agents: []string{"A mason builds upload."}})
	p.await("the changed workstream marked", `document.querySelector(`+quote(streamRow+"[data-field=unread]")+`) !== null`)
	p.await("the shown workstream unmarked", `document.querySelector(`+quote(`[data-select="`+string(quiet)+`"] [data-field=unread]`)+`) === null`)
	sameOrder("a status change on the workstream not shown")

	// Looking at it clears the mark, without moving it.
	p.selectWorkstream(stream)
	p.await("the mark cleared", `document.querySelector('[data-field=unread]') === null`)
	sameOrder("clearing the mark")

	p.run(chromedp.Reload())
	p.await("the live connection after the reload", `document.body.dataset.connection === 'live'`)
	p.await("the reloaded workstream shown", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(stream)))
	settle()
	p.await("nothing marked after the reload", `document.querySelector('[data-field=unread]') === null`)
	sameOrder("the reload")
}
