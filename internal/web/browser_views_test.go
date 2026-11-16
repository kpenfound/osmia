package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// answers wraps the fake API with the routes these tests control: the
// abandon endpoint, whose request bodies it records, and the feed, which it
// can make fail.
type answers struct {
	mu        sync.Mutex
	abandoned []string
	failFeed  atomic.Bool
	next      http.Handler
}

func (a *answers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/abandon/"):
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.abandoned = append(a.abandoned, r.URL.Path+" "+string(body))
		a.mu.Unlock()
		writeJSON(w, `{}`)
	case strings.HasPrefix(r.URL.Path, "/v1/feed/") && a.failFeed.Load():
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"code":"internal","message":"the trace is not readable"}}`)
	default:
		a.next.ServeHTTP(w, r)
	}
}

func (a *answers) requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.abandoned...)
}

func openViewsPage(t *testing.T, workstreams int, width, height int64, failFeed bool) (*page, *fakeAPI, *answers) {
	t.Helper()
	api := newFakeAPI(workstreams)
	a := &answers{next: api.handler()}
	a.failFeed.Store(failFeed)
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(width, height), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	return p, api, a
}

// Abandoning opens a dialog in the page. It names the consequence, asks for a
// reason, confirms with the danger button and cancels with a quiet one, closes
// on Escape and hands focus back to the control that opened it. The page
// never asks the browser to confirm and never jumps to Documents.
func TestBrowserAbandonOpensADialogTheKeyboardOperates(t *testing.T) {
	t.Parallel()
	p, _, a := openViewsPage(t, 1, 1280, 800, false)
	p.click(`[data-select="w_000"]`)
	p.await("the workstream shown", `document.getElementById('workstream-head').dataset.workstream === 'w_000'`)
	p.eval(`window.nativeConfirms = 0; window.confirm = () => { window.nativeConfirms++; return true; }`, nil)
	open := func() {
		t.Helper()
		p.click("#workstream-menu-button")
		p.click(`[data-workstream-action="abandon"]`)
		p.await("the dialog open", `document.getElementById('confirm-dialog').open`)
	}

	open()
	p.await("the consequence named, the danger button and a reason field focused", `(() => {
		const dialog = document.getElementById('confirm-dialog');
		const accept = document.getElementById('confirm-accept');
		return dialog.querySelector('h2').textContent === 'Abandon Workstream 0?' &&
			dialog.textContent.includes('Abandoning stops new turns') &&
			accept.classList.contains('danger') && accept.textContent === 'Abandon workstream' &&
			document.getElementById('confirm-cancel').classList.contains('quiet') &&
			document.activeElement === document.getElementById('confirm-reason') &&
			document.querySelector('[data-panel=documents]').hidden &&
			document.querySelector('[data-tab=conversation]').getAttribute('aria-selected') === 'true';
	})()`)

	// Escape closes it and focus returns to the menu button.
	p.run(chromedp.KeyEvent(kb.Escape))
	p.await("the dialog closed and focus returned", `!document.getElementById('confirm-dialog').open && document.activeElement === document.getElementById('workstream-menu-button')`)
	if got := a.requests(); len(got) != 0 {
		t.Fatalf("a cancelled abandon reached the service: %v", got)
	}

	// Tab moves through the reason, cancel and the danger button; the danger
	// button without a reason keeps the dialog open and says why.
	open()
	p.run(chromedp.KeyEvent(kb.Tab))
	p.await("cancel after the reason", `document.activeElement === document.getElementById('confirm-cancel')`)
	p.run(chromedp.KeyEvent(kb.Tab))
	p.await("the danger button after cancel", `document.activeElement === document.getElementById('confirm-accept')`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.awaitText("#confirm-result", "Give a reason")
	p.await("the dialog still open on the reason", `document.getElementById('confirm-dialog').open && document.activeElement === document.getElementById('confirm-reason')`)

	// With a reason, Tab Tab Enter confirms.
	p.typeInto("#confirm-reason", "Superseded by the upload design.")
	p.run(chromedp.KeyEvent(kb.Tab), chromedp.KeyEvent(kb.Tab))
	p.await("the danger button focused", `document.activeElement === document.getElementById('confirm-accept')`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.await("the dialog closed", `!document.getElementById('confirm-dialog').open`)
	p.awaitText("#inbox-result", "Abandoned Workstream 0.")
	got := a.requests()
	if len(got) != 1 || got[0] != `/v1/abandon/w_000 {"reason":"Superseded by the upload design."}` {
		t.Fatalf("abandon requests: %v", got)
	}
	var native int
	p.eval(`window.nativeConfirms`, &native)
	if native != 0 {
		t.Fatalf("the page called window.confirm %d times", native)
	}
}

// The create forms share one layout: each label sits above its control, hints
// follow the control, the actions come last and the form-level result after
// them, with the same spacing in all four.
func TestBrowserCreateFormsShareOneLayout(t *testing.T) {
	t.Parallel()
	p, _, _ := openViewsPage(t, 1, 1280, 800, false)
	for _, item := range []struct{ open, form string }{
		{"handin", "#handin-form"},
		{"design-add", "#design-add-form"},
		{"session-add", "#session-add-form"},
		{"project-add", "#project-add"},
	} {
		p.click("#new-workstream")
		p.click(`#create-menu [data-open="` + item.open + `"]`)
		p.await(item.open+" shown", `!document.querySelector('.view[data-view="`+item.open+`"]').hidden`)
		var layout struct {
			Class        string
			Gap          string
			Labels       int
			LabelAbove   bool
			LabelsLinked bool
			ActionsLast  bool
			ResultLast   bool
			Width        float64
		}
		p.eval(`(() => {
			const form = document.querySelector(`+quote(item.form)+`);
			const fields = [...form.querySelectorAll('.field')];
			const above = fields.every((f) => {
				const label = f.querySelector(':scope > label');
				const control = f.querySelector('input:not([type=checkbox]), select, textarea');
				return !control || label.getBoundingClientRect().bottom <= control.getBoundingClientRect().top + 1;
			});
			const linked = fields.every((f) => {
				const label = f.querySelector(':scope > label');
				return document.getElementById(label.htmlFor) !== null && f.contains(document.getElementById(label.htmlFor));
			});
			const kids = [...form.children];
			return {
				Class: form.className, Gap: getComputedStyle(form).rowGap, Labels: fields.length, LabelAbove: above, LabelsLinked: linked,
				ActionsLast: kids[kids.length - 2].classList.contains('form-actions'),
				ResultLast: kids[kids.length - 1].classList.contains('result'),
				Width: form.getBoundingClientRect().width,
			};
		})()`, &layout)
		if layout.Class != "form" || layout.Gap != "16px" || layout.Labels == 0 || !layout.LabelAbove || !layout.LabelsLinked || !layout.ActionsLast || !layout.ResultLast || layout.Width > 641 {
			t.Fatalf("%s layout: %+v", item.form, layout)
		}
	}
}

// A failed read is a designed state with a way forward, and the banner names
// what could not be read instead of a request path; trying again reads it.
func TestBrowserFailedReadsOfferTheNextStep(t *testing.T) {
	t.Parallel()
	p, api, a := openViewsPage(t, 1, 1280, 800, true)
	p.click(`[data-select="w_000"]`)
	p.await("the workstream shown", `document.getElementById('workstream-head').dataset.workstream === 'w_000'`)
	p.await("a conversation error with a way to retry", `(() => {
		const state = document.querySelector('#workstreams [data-state=error]');
		return state && state.getAttribute('role') === 'alert' && state.textContent.includes('The conversation is unavailable.') &&
			state.querySelector('button[data-action=retry]') !== null;
	})()`)
	p.await("a banner naming the view and keeping the raw path out", `(() => {
		const banner = document.getElementById('problems');
		return !banner.hidden && banner.textContent.includes('Could not read the conversation of Workstream 0.') &&
			banner.textContent.includes('the trace is not readable') && !banner.textContent.includes('/feed/') && !banner.textContent.includes('Cannot read');
	})()`)

	a.failFeed.Store(false)
	p.click("#workstreams [data-state=error] button[data-action=retry]")
	p.await("the conversation read again", `document.querySelector('#workstreams [data-state=error]') === null && document.getElementById('problems').hidden &&
		document.querySelector('#workstreams [data-field=empty] [data-state=empty]') !== null`)
	p.awaitText("#workstreams [data-field=empty]", "Write to the chief of staff below.")

	// The banner offers the same way forward for a read that fails later.
	a.failFeed.Store(true)
	api.pushEvent("resync")
	p.await("the banner and its retry after the read failed again", `!document.getElementById('problems').hidden && !document.getElementById('problems-retry').hidden`)
	a.failFeed.Store(false)
	p.click("#problems-retry")
	p.await("the banner gone once the conversation reads", `document.getElementById('problems').hidden && document.querySelector('#workstreams [data-state=error]') === null`)
}

// Empty lists say what belongs there and offer the control that creates it.
func TestBrowserEmptyListsOfferTheNextStep(t *testing.T) {
	t.Parallel()
	p, _, _ := openViewsPage(t, 0, 1280, 800, false)
	p.await("the sidebar's empty state with a button that opens hand-in", `(() => {
		const state = document.querySelector('#workstream-list [data-state=empty]');
		return state && state.textContent.includes('No workstreams yet.') && state.querySelector('button').textContent === 'Hand in a feature';
	})()`)
	p.click("#workstream-list [data-state=empty] button")
	p.await("hand-in opened", `!document.querySelector('.view[data-view=handin]').hidden`)

	p.click("#project-home")
	p.await("the project pane's empty state with a button that opens project creation", `(() => {
		const state = document.querySelector('#project-pane [data-state=empty]');
		return state && state.textContent.includes('No projects yet.') && state.querySelector('button.primary').textContent === 'Create project';
	})()`)
	p.click("#project-pane [data-state=empty] button")
	p.await("project creation opened", `!document.querySelector('.view[data-view=project-add]').hidden`)
}

// While the workstream list is read the sidebar holds a loading state in the
// space the list will take.
func TestBrowserLoadingStateHoldsTheSidebar(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(2)
	release := api.holdStatus()
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("a loading state with bars", `(() => {
		const state = document.querySelector('#workstream-list [data-state=loading]');
		return state && state.getAttribute('role') === 'status' && state.querySelectorAll('.state-bars > span').length === 3 &&
			state.textContent.includes('Loading workstreams…');
	})()`)
	release()
	p.await("the list replaces it", `document.querySelectorAll('#workstream-list [data-select]').length === 2 && document.querySelector('#workstream-list [data-state]') === null`)
}
