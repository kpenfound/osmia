package browsertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// BrowserTimeout bounds each wait for the Page to show something.
const BrowserTimeout = 30 * time.Second

// Page drives one headless Chromium tab and records every URL it requests.
type Page struct {
	T         *testing.T
	Ctx       context.Context
	Mu        sync.Mutex
	Requested []string
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

// OpenBrowser gives the test its own tab, on the Chromium binary
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
func OpenBrowser(t *testing.T) *Page {
	t.Helper()
	binary := os.Getenv("OSMIA_BROWSER")
	if binary == "" {
		t.Skip("OSMIA_BROWSER names no Chromium binary; dagger check runs the browser tests")
	}
	root := sharedBrowser(t, binary)
	ctx, cancel := chromedp.NewContext(root)
	t.Cleanup(cancel)
	p := &Page{T: t, Ctx: ctx}
	chromedp.ListenTarget(ctx, func(event any) {
		if e, ok := event.(*network.EventRequestWillBeSent); ok {
			p.Mu.Lock()
			p.Requested = append(p.Requested, e.Request.URL)
			p.Mu.Unlock()
		}
	})
	// The first run opens this test's own tab, on its own long-lived
	// context, before any later action's per-run timeout, and brings it to
	// the front before anything else can observe its visibility.
	fixturekit.Must(t, chromedp.Run(ctx, network.Enable(), cdppage.BringToFront()))
	return p
}

// run runs the actions, and fails with the Page's text when one fails.
func (p *Page) Run(actions ...chromedp.Action) {
	p.T.Helper()
	ctx, cancel := context.WithTimeout(p.Ctx, BrowserTimeout)
	defer cancel()
	if err := chromedp.Run(ctx, actions...); err != nil {
		var text string
		shown, cancel := context.WithTimeout(p.Ctx, 5*time.Second)
		defer cancel()
		chromedp.Run(shown, chromedp.Evaluate("document.body?.innerText ?? ''", &text))
		p.T.Fatalf("%v; the Page shows:\n%s", err, text)
	}
}

func (p *Page) Eval(expression string, out any) {
	p.T.Helper()
	p.Run(chromedp.Evaluate(expression, out))
}

// await waits until the expression is true, and fails with the Page's text
// when it stays false.
func (p *Page) Await(what, expression string) {
	p.T.Helper()
	deadline := time.Now().Add(BrowserTimeout)
	for {
		var ok bool
		p.Eval("!!("+expression+")", &ok)
		if ok {
			return
		}
		if time.Now().After(deadline) {
			var text, connection string
			p.Eval("document.body?.innerText ?? ''", &text)
			p.Eval("document.body?.dataset.connection ?? ''", &connection)
			p.T.Fatalf("the Page never showed %s (connection %s):\n%s", what, connection, text)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Quote returns s as a JavaScript string literal.
func Quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TextOf is an expression for the text of the first element selector
// matches, or the empty string without one.
func TextOf(selector string) string {
	return "(document.querySelector(" + Quote(selector) + ")?.textContent ?? '')"
}

// awaitText waits until the element's text contains want.
func (p *Page) AwaitText(selector, want string) {
	p.T.Helper()
	p.Await(fmt.Sprintf("%q in %s", want, selector), TextOf(selector)+".includes("+Quote(want)+")")
}

// click clicks the first element selector matches, once it is scrolled to
// the middle of the Page, clear of the sticky heading and message field.
func (p *Page) Click(selector string) {
	p.T.Helper()
	p.Run(ClickCurrent(selector))
}

// ClickCurrent reselects an element replaced during lookup or mouse
// dispatch. A capture listener confirms delivery before the page's handler
// can replace the element, so an accepted action is never submitted twice.
func ClickCurrent(selector string, opts ...chromedp.QueryOption) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := chromedp.Evaluate(`(() => {
				const probe = window.__osmiaTestClick = { received: false };
				probe.listen = e => {
					if (e.composedPath().some(n => n instanceof Element && n.matches(`+Quote(selector)+`))) probe.received = true;
				};
				document.addEventListener('click', probe.listen, true);
				document.querySelector(`+Quote(selector)+`)?.scrollIntoView({block: 'center'});
			})()`, nil).Do(ctx); err != nil {
				return err
			}
			err := chromedp.Click(selector, append([]chromedp.QueryOption{chromedp.ByQuery}, opts...)...).Do(ctx)
			var received bool
			cleanup := chromedp.Evaluate(`(() => {
				const probe = window.__osmiaTestClick;
				document.removeEventListener('click', probe.listen, true);
				delete window.__osmiaTestClick;
				return probe.received;
			})()`, &received).Do(ctx)
			if err == nil {
				if cleanup != nil || received {
					return cleanup
				}
				// Replacing a pressed node can deliver the click to its
				// ancestor instead. No handler on the target ran.
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			var protocol *cdproto.Error
			if !errors.As(err, &protocol) || protocol.Code != -32000 ||
				(protocol.Message != "Could not find node with given id" && protocol.Message != "No node with given id found") {
				return err
			}
		}
	})
}

// typeInto types text into the first field selector matches.
func (p *Page) TypeInto(selector, text string) {
	p.T.Helper()
	p.Run(chromedp.SendKeys(selector, text, chromedp.ByQuery))
}

// choose selects value in the first select selector matches.
func (p *Page) Choose(selector, value string) {
	p.T.Helper()
	p.Run(chromedp.SetValue(selector, value, chromedp.ByQuery))
}

// selectWorkstream picks a workstream from the list the way the owner does,
// opening the list first where it is collapsed, and waits until the main
// area shows it.
func (p *Page) SelectWorkstream(id config.WorkstreamID) {
	p.T.Helper()
	row := `[data-select="` + string(id) + `"]`
	p.Await("the workstream "+string(id)+" in the list", `document.querySelector(`+Quote(row)+`) !== null`)
	var collapsed bool
	p.Eval(`getComputedStyle(document.getElementById('sidebar')).display === 'none'`, &collapsed)
	if collapsed {
		p.Click("#picker-toggle")
	}
	p.Run(selectSidebarRow(row, `!document.querySelector('.view[data-view="workstream"]').hidden && document.getElementById('workstream-head').dataset.workstream === `+Quote(string(id))+
		` && document.body.dataset.picker === 'closed'`))
}

// sidebarIDs reads the workstream IDs the sidebar's list of work shows, top
// to bottom.
func (p *Page) SidebarIDs() []string {
	p.T.Helper()
	var joined string
	p.Eval(`[...document.querySelectorAll('#workstream-list [data-select]')].map((e) => e.dataset.select).join(',')`, &joined)
	if joined == "" {
		return nil
	}
	return strings.Split(joined, ",")
}

// awaitSidebarOrder waits until the sidebar's list of work shows exactly
// want, top to bottom.
func (p *Page) AwaitSidebarOrder(want ...config.WorkstreamID) {
	p.T.Helper()
	ids := make([]string, len(want))
	for i, w := range want {
		ids[i] = string(w)
	}
	joined := strings.Join(ids, ",")
	expr := `[...document.querySelectorAll('#workstream-list [data-select]')].map((e) => e.dataset.select).join(',') === ` + Quote(joined)
	p.Await("the sidebar ordered "+joined, expr)
}

// closePopovers closes the header's open popovers and menus.
func (p *Page) ClosePopovers() {
	p.T.Helper()
	p.Eval(`document.querySelectorAll(':popover-open').forEach((e) => e.hidePopover())`, nil)
}

// openView opens one of the views the settings menu lists.
func (p *Page) OpenView(name string) {
	p.T.Helper()
	p.ClosePopovers()
	p.Click("#settings-button")
	p.Click(`#settings-menu [data-open="` + name + `"]`)
	p.Await("the "+name+" view", `!document.querySelector('.view[data-view="`+name+`"]').hidden`)
}

// openPauses opens the header's pause popover unless it is open.
func (p *Page) OpenPauses() {
	p.T.Helper()
	var open bool
	p.Eval(`document.getElementById('pause-popover').matches(':popover-open')`, &open)
	if !open {
		p.Click("#pause-button")
		p.Await("the pauses", `document.getElementById('pause-popover').matches(':popover-open')`)
	}
}

// paths returns the paths of every URL the Page requested.
func (p *Page) Paths() []string {
	p.T.Helper()
	p.Mu.Lock()
	defer p.Mu.Unlock()
	var out []string
	for _, raw := range p.Requested {
		u, err := url.Parse(raw)
		fixturekit.Must(p.T, err)
		out = append(out, u.Path)
	}
	return out
}

// LinkProxy forwards loopback connections to the web listener. cut ends
// every forwarded connection and refuses new ones until restore.
type LinkProxy struct {
	Listener net.Listener
	Target   string
	Mu       sync.Mutex
	Down     bool
	Conns    map[net.Conn]struct{}
}

func NewLinkProxy(t *testing.T, target string) *LinkProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	fixturekit.Must(t, err)
	p := &LinkProxy{Listener: l, Target: target, Conns: map[net.Conn]struct{}{}}
	go func() {
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			go p.Forward(client)
		}
	}()
	t.Cleanup(func() {
		l.Close()
		p.Cut()
	})
	return p
}

func (p *LinkProxy) Forward(client net.Conn) {
	server, err := net.Dial("tcp", p.Target)
	if err != nil {
		client.Close()
		return
	}
	p.Mu.Lock()
	if p.Down {
		p.Mu.Unlock()
		client.Close()
		server.Close()
		return
	}
	p.Conns[client], p.Conns[server] = struct{}{}, struct{}{}
	p.Mu.Unlock()
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
	p.Mu.Lock()
	delete(p.Conns, client)
	delete(p.Conns, server)
	p.Mu.Unlock()
}

func (p *LinkProxy) Cut() {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.Down = true
	for c := range p.Conns {
		c.Close()
	}
}

func (p *LinkProxy) Restore() {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.Down = false
}

// Shutdown closes the Chromium process after all tests in this package finish.
func Shutdown() {
	if sharedBrowserDone != nil {
		sharedBrowserDone()
	}
}

func (p *Page) SelectDesign(id config.WorkstreamID) {
	p.T.Helper()
	row := `[data-select="` + string(id) + `"]`
	p.Await("the design "+string(id)+" in the list", `document.querySelector(`+Quote(row)+`) !== null`)
	var collapsed bool
	p.Eval(`getComputedStyle(document.getElementById('sidebar')).display === 'none'`, &collapsed)
	if collapsed {
		p.Click("#picker-toggle")
	}
	p.Run(selectSidebarRow(row, `!document.querySelector('.view[data-view="design"]').hidden &&
		document.getElementById('design-meta').textContent.includes(`+Quote(string(id))+`)`))
}

// selectSidebarRow repeats this idempotent selection when a sidebar reorder
// disconnects the focused button before the trusted key event reaches it.
func selectSidebarRow(selector, shown string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		for {
			if err := chromedp.Evaluate(`document.querySelector(`+Quote(selector)+`)?.focus()`, nil).Do(ctx); err != nil {
				return err
			}
			if err := chromedp.KeyEvent(kb.Enter).Do(ctx); err != nil {
				return err
			}
			var selected bool
			if err := chromedp.Evaluate(shown, &selected).Do(ctx); err != nil || selected {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
}
