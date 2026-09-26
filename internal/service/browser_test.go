package service

import (
	"context"
	"encoding/json"
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
	"github.com/chromedp/chromedp"
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

// openBrowser starts the Chromium binary OSMIA_BROWSER names, or skips the
// test without one. The browser Dagger check sets it.
func openBrowser(t *testing.T) *page {
	t.Helper()
	binary := os.Getenv("OSMIA_BROWSER")
	if binary == "" {
		t.Skip("OSMIA_BROWSER names no Chromium binary; dagger check runs the browser tests")
	}
	options := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(binary), chromedp.NoSandbox,
		chromedp.Flag("disable-dev-shm-usage", true))
	allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(), options...)
	ctx, cancel := chromedp.NewContext(allocator)
	t.Cleanup(func() {
		cancel()
		cancelAllocator()
	})
	p := &page{t: t, ctx: ctx}
	chromedp.ListenTarget(ctx, func(event any) {
		if e, ok := event.(*network.EventRequestWillBeSent); ok {
			p.mu.Lock()
			p.requested = append(p.requested, e.Request.URL)
			p.mu.Unlock()
		}
	})
	// The first run starts the browser for the lifetime of ctx.
	must(t, chromedp.Run(ctx, network.Enable()))
	return p
}

func (p *page) run(actions ...chromedp.Action) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(p.ctx, browserTimeout)
	defer cancel()
	must(p.t, chromedp.Run(ctx, actions...))
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

// click clicks the first element selector matches.
func (p *page) click(selector string) {
	p.t.Helper()
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
	live := s.active.repository
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
	_, err := f.s.active.repository.SetStatus(context.Background(), trace.ChiefOfStaff, f.chief, content, time.Now().UTC(), func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	must(t, err)
}

// The page, opened through the web listener, shows each workstream's goal,
// attention, note, units by state and sessions, the capacity and the pauses
// from the /v1 views; it follows a status change without a reload, at phone
// and laptop widths, and after its event stream is lost it reconnects and
// reads again what changed meanwhile.
func TestBrowserPageShowsActiveWorkAndStaysCurrent(t *testing.T) {
	p := openBrowser(t)
	f := newPageFixture(t)
	card := `[data-workstream="` + string(stream) + `"] `
	quietCard := `[data-workstream="` + string(quiet) + `"] `
	pause := `[data-pause="workstream:` + string(quiet) + `"] `

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.awaitText(card+"[data-field=goal]", "Ship resumable uploads.")
	for selector, want := range map[string]string{
		card + "[data-field=state]":                              "building",
		card + "[data-field=note]":                               "A mason is building the upload unit.",
		card + `[data-state=merged]`:                             "merged 1 resume",
		card + `[data-state=implementing]`:                       "implementing 1 upload",
		card + `[data-state=ready]`:                              "ready 1 audit",
		card + `[data-state=planned]`:                            "planned 1 index",
		card + `[data-field=agents] [data-role=mason]`:           "mason on upload · default · running",
		card + `[data-role=chief_of_staff] [data-field=profile]`: "other",
		`#capacity [data-role=mason] [data-field=slots]`:         "1 / 1 slots",
		`#capacity [data-role=mason] [data-field=waiting]`:       string(stream) + " unit audit: every slot is taken",
		`#capacity [data-role=reviewer]`:                         "Nothing waits.",
		pause + "[data-field=scope]":                             "Workstream " + string(quiet),
		pause + "[data-field=reason]":                            "The owner is travelling",
		pause + "[data-field=source]":                            "the owner",
		quietCard + "[data-field=goal]":                          "No status yet",
	} {
		p.awaitText(selector, want)
	}
	var shown bool
	p.eval(`document.querySelector(`+quote(card+"[data-field=attention]")+`) !== null || !document.getElementById('problems').hidden`, &shown)
	if shown {
		t.Fatal("the page shows an attention or a problem the views do not hold")
	}

	// A status change appears without a reload.
	p.eval(`window.notReloaded = true`, nil)
	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads with dedupe.", Attention: "Rule on the upload API.", Note: "Upload waits for a ruling.", Agents: []string{"A mason builds upload."}})
	p.awaitText(card+"[data-field=goal]", "Ship resumable uploads with dedupe.")
	p.awaitText(card+"[data-field=attention]", "Rule on the upload API.")
	p.awaitText(card+"[data-field=note]", "Upload waits for a ruling.")
	p.await("the same document", `window.notReloaded === true`)

	// Laptop widths lay the workstreams side by side and phone widths stack
	// them; neither scrolls sideways.
	layout := `(() => {
		const cards = [...document.querySelectorAll('[data-workstream]')].map((c) => c.getBoundingClientRect());
		return {
			overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
			fits: cards.every((r) => r.left >= 0 && r.right <= document.documentElement.clientWidth),
			sideBySide: cards.length === 2 && cards[0].top === cards[1].top,
		};
	})()`
	var laptop, phone struct{ Overflow, Fits, SideBySide bool }
	p.eval(layout, &laptop)
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile))
	p.eval(layout, &phone)
	if laptop.Overflow || !laptop.Fits || !laptop.SideBySide || phone.Overflow || !phone.Fits || phone.SideBySide {
		t.Fatalf("layout at laptop width %+v, at phone width %+v", laptop, phone)
	}
	p.await("the same document", `window.notReloaded === true`)

	// The stream is lost; what changes meanwhile is read once the page
	// reconnects.
	link := newLinkProxy(t, f.s.WebAddr())
	p.run(chromedp.Navigate("http://" + link.listener.Addr().String() + "/"))
	p.await("the live connection through the link", `document.body.dataset.connection === 'live'`)
	p.awaitText(card+"[data-field=goal]", "Ship resumable uploads with dedupe.")
	p.eval(`window.notReloaded = true`, nil)
	link.cut()
	p.await("the lost connection", `document.body.dataset.connection === 'lost'`)
	f.status(t, trace.StatusContent{Goal: "Ship resumable uploads with dedupe.", Note: "The ruling came; upload continues.", Agents: []string{"A mason builds upload."}})
	mutation(t, f.c, "DELETE", "pause", ClearPauseRequest{Scope: "workstream", Project: project, Workstream: quiet})
	var stale string
	p.eval(textOf(card+"[data-field=note]"), &stale)
	if stale != "Upload waits for a ruling." {
		t.Fatalf("the page changed while its stream was lost: %q", stale)
	}
	link.restore()
	p.await("the live connection after the loss", `document.body.dataset.connection === 'live'`)
	p.awaitText(card+"[data-field=note]", "The ruling came; upload continues.")
	p.await("no attention", `document.querySelector(`+quote(card+"[data-field=attention]")+`) === null`)
	p.awaitText("#pause-list", "Nothing is paused.")
	p.await("the same document", `window.notReloaded === true`)

	// A pause set while the stream is live appears without a reload.
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: project, Workstream: stream}, Mode: "soft", Reason: "Hold the uploads", Source: runtime.PauseOwner})
	livePause := `[data-pause="workstream:` + string(stream) + `"] `
	p.awaitText(livePause+"[data-field=reason]", "Hold the uploads")
	p.awaitText(livePause+"[data-field=source]", "the owner")
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
