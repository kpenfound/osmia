package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// browserTimeout bounds each wait for the page to show something.
const browserTimeout = 30 * time.Second

// page drives one headless Chromium tab.
type page struct {
	t   *testing.T
	ctx context.Context
}

// requireBrowser skips t without failing it when OSMIA_BROWSER names no
// Chromium binary; the browser Dagger check sets it.
func requireBrowser(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("OSMIA_BROWSER")
	if binary == "" {
		t.Skip("OSMIA_BROWSER names no Chromium binary; dagger check runs the browser tests")
	}
	return binary
}

// openBrowser starts the Chromium binary OSMIA_BROWSER names, or skips the
// test without one.
func openBrowser(t *testing.T) *page {
	t.Helper()
	binary := requireBrowser(t)
	options := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(binary), chromedp.NoSandbox,
		chromedp.Flag("disable-dev-shm-usage", true))
	allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(), options...)
	ctx, cancel := chromedp.NewContext(allocator)
	t.Cleanup(func() {
		cancel()
		cancelAllocator()
	})
	// The first run starts the browser for the lifetime of ctx, before any
	// per-action context.WithTimeout wraps it.
	if err := chromedp.Run(ctx); err != nil {
		t.Fatalf("starting the browser: %v", err)
	}
	return &page{t: t, ctx: ctx}
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
			var text string
			p.eval("document.body?.innerText ?? ''", &text)
			p.t.Fatalf("the page never showed %s:\n%s", what, text)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// quote returns s as a JavaScript string literal.
func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// awaitText waits until the first element selector matches contains want.
func (p *page) awaitText(selector, want string) {
	p.t.Helper()
	expr := "(document.querySelector(" + quote(selector) + `)?.textContent ?? '').includes(` + quote(want) + `)`
	p.await(want+" in "+selector, expr)
}

// fakeAPI serves the page's own files, through Serve, beside a minimal,
// hand-built /v1 API: just enough of every view the first page load reads
// for the page to render without error, with no backing service. It counts
// the requests made for the workstream list and can hold its response open
// until a test chooses to let it go, so tests can see what the page does
// before that response arrives, and how many requests it took.
type fakeAPI struct {
	workstreams int

	mu         sync.Mutex
	statusHits int
	hold       chan struct{}
}

func newFakeAPI(workstreams int) *fakeAPI {
	return &fakeAPI{workstreams: workstreams}
}

// holdStatus makes every /v1/status response already in flight, and every
// one requested until release is called, wait for release.
func (f *fakeAPI) holdStatus() (release func()) {
	ch := make(chan struct{})
	f.mu.Lock()
	f.hold = ch
	f.mu.Unlock()
	return func() { close(ch) }
}

func (f *fakeAPI) statusRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusHits
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, body)
}

func handleJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, body)
	}
}

// handleStatus answers the workstream list: a fixed shape with as many
// workstreams as the test asked for, each with enough of a status for the
// page to show it without reading anything per workstream.
func (f *fakeAPI) handleStatus(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.statusHits++
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	var workstreams strings.Builder
	for i := 0; i < f.workstreams; i++ {
		if i > 0 {
			workstreams.WriteString(",")
		}
		fmt.Fprintf(&workstreams, `{"workstream":"w_%03d","project":"p_fake","workspaces":"git","state":"building",`+
			`"units":[],"advisories":[],"open_questions":0,"gates":[],"context_mode":"file","drift":null,`+
			`"status":{"goal":"Workstream %d","attention":"","note":"","agents":[],"revision":1,"updated_at":"2026-01-01T00:00:00Z"},"agents":[]}`, i, i)
	}
	writeJSON(w, `{"workstreams":[`+workstreams.String()+`],"profiles":{},"daily_budget":null,"capacity":null,`+
		`"provider_usage":null,"diagnostics":[],"workspaces":{"setting":"git","backend":"git"}}`)
}

// handleFeed answers any workstream's feed with none of its history, which
// is enough for the page to show it without error.
func (f *fakeAPI) handleFeed(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/feed/")
	writeJSON(w, fmt.Sprintf(`{"workstream":%s,"entries":[]}`, quote(id)))
}

// handleEvents answers the event stream with a single resync and then holds
// the connection open, the way the real service does until the page closes
// it or the stream is lost.
func (f *fakeAPI) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "event: resync\ndata: {}\n\n")
	flusher.Flush()
	<-r.Context().Done()
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", f.handleStatus)
	mux.HandleFunc("/v1/runtime", handleJSON(`{"effective":{"pauses":[],"priorities":[]},"profiles":{},"diagnostics":[]}`))
	mux.HandleFunc("/v1/config", handleJSON(`{"root":"","digest":"fake","effective":{"profiles":{}},"project":null,"projects":[],"diagnostics":[],"drift":{"differs":false,"files":[]}}`))
	mux.HandleFunc("/v1/inbox", handleJSON(`{"entries":[]}`))
	mux.HandleFunc("/v1/charter", handleJSON(`{"proposals":[]}`))
	mux.HandleFunc("/v1/feed/", f.handleFeed)
	mux.HandleFunc("/v1/events", f.handleEvents)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !Serve(w, r) {
			http.NotFound(w, r)
		}
	})
	return mux
}

// The page draws its layout and a visible loading state for the workstream
// list immediately, before the workstream list's response arrives, and
// shows the list once it does.
func TestBrowserFirstLoadShowsLayoutAndLoadingBeforeTheListArrives(t *testing.T) {
	api := newFakeAPI(3)
	release := api.holdStatus()
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	// The browser holds the event stream open, so it must be torn down
	// (as a later-registered, and so earlier-run, cleanup) before the
	// server closes, or Close blocks on that connection.
	p := openBrowser(t)

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))

	// The layout and a visible loading state show up before the held
	// response arrives.
	p.await("the sidebar and main layout", `document.getElementById('sidebar') !== null && document.getElementById('main') !== null &&
		getComputedStyle(document.getElementById('sidebar')).display !== 'none'`)
	p.awaitText(`#workstream-list [data-field=loading]`, "Loading")
	var listed int
	p.eval(`document.querySelectorAll('#workstream-list [data-select]').length`, &listed)
	if listed != 0 {
		t.Fatalf("the list showed %d workstreams before the response arrived", listed)
	}

	release()
	p.await("three workstreams listed", `document.querySelectorAll('#workstream-list [data-select]').length === 3`)
	p.await("the loading state gone", `document.querySelector('#workstream-list [data-field=loading]') === null`)
}

// On first page load, the page requests the workstream list the same number
// of times whether there is one workstream or twenty: the request does not
// grow with the number of workstreams.
func TestBrowserFirstLoadRequestsTheWorkstreamListInBoundedRequests(t *testing.T) {
	requireBrowser(t)
	requests := func(t *testing.T, workstreams int) int {
		api := newFakeAPI(workstreams)
		server := httptest.NewServer(api.handler())
		t.Cleanup(server.Close)
		// The browser holds the event stream open, so it must be torn
		// down (as a later-registered, and so earlier-run, cleanup)
		// before the server closes, or Close blocks on that connection.
		p := openBrowser(t)

		p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
		p.await("every workstream listed", fmt.Sprintf(`document.querySelectorAll('#workstream-list [data-select]').length === %d`, workstreams))
		// Let any further, unwanted requests the first render might still
		// trigger settle before counting.
		time.Sleep(200 * time.Millisecond)
		return api.statusRequests()
	}

	var one, twenty int
	t.Run("one workstream", func(t *testing.T) { one = requests(t, 1) })
	t.Run("twenty workstreams", func(t *testing.T) { twenty = requests(t, 20) })
	if one != twenty {
		t.Fatalf("the workstream list was requested %d times for 1 workstream and %d times for 20", one, twenty)
	}
}
