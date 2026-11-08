package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
)

// archivedGroupAPI serves a workgroup whose members are all archived. Its
// shared feed is readable until gone is set.
type archivedGroupAPI struct {
	base *fakeAPI
	mu   sync.Mutex
	gone bool
	// sent counts the POSTs to the workgroup, which a read-only view never makes.
	sent int
}

const archivedGroupFeed = `{"workgroup":"g_test","members":[` +
	`{"workstream":"w_003","repo":"p_fake","state":"delivered","display_state":"delivered","units":{},"wait":"","pr_link":"https://example.invalid/pull/1","running":false,"archived":true,"archived_by":"chief_of_staff"},` +
	`{"workstream":"w_004","repo":"p_fake","state":"delivered","display_state":"delivered","units":{},"wait":"","pr_link":"https://example.invalid/pull/2","running":false,"archived":true}],` +
	`"entries":[` +
	`{"kind":"message","turn":"owner-1","text":"How is the shared interface going?","state":"done","at":"2026-01-01T00:00:00Z"},` +
	`{"kind":"response","turn":"owner-1","text":"Both repositories agreed on the shared interface.","state":"done","at":"2026-01-01T00:00:01Z"},` +
	`{"kind":"action","turn":"archive-w_004","text":"Archived workstream w_004 because its pull request merged: https://example.invalid/pull/2","state":"done","at":"2026-01-01T00:00:02Z","workstream":"w_004","repo":"p_fake"}]}`

func (a *archivedGroupAPI) handler(members ...string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		var rows []string
		for _, id := range members {
			rows = append(rows, fmt.Sprintf(`{"workstream":%q,"project":"p_fake","workspaces":"git","state":"delivered","display_state":"delivered",`+
				`"archived":true,"archived_by":"chief_of_staff","trace_deleted":true,"workgroup":{"id":"g_test","members":["w_003","w_004"],"edges":[]},`+
				`"units":[],"advisories":[],"open_questions":0,"gates":[],"context_mode":"file","drift":null,"status":null,"agents":[]}`, id))
		}
		writeJSON(w, `{"workstreams":[`+strings.Join(rows, ",")+`],"profiles":{},"daily_budget":null,"capacity":null,`+
			`"provider_usage":null,"diagnostics":[],"workspaces":{"setting":"git","backend":"git"}}`)
	})
	mux.HandleFunc("/v1/workgroups/g_test/feed", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		gone := a.gone
		a.mu.Unlock()
		if gone {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"validation","message":"workgroup g_test is not in an active project"}}`)
			return
		}
		writeJSON(w, archivedGroupFeed)
	})
	mux.HandleFunc("/v1/workgroups/g_test/conversation", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.sent++
		a.mu.Unlock()
		http.Error(w, "no live member", http.StatusConflict)
	})
	mux.Handle("/", a.base.handler())
	return mux
}

func openArchivedGroupPage(t *testing.T, a *archivedGroupAPI, members ...string) *page {
	t.Helper()
	server := httptest.NewServer(a.handler(members...))
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	return p
}

// A workgroup whose members are all archived is reached from the list and read
// as a conversation that cannot be written to.
func TestBrowserArchivedWorkgroupShowsItsRetainedConversationReadOnly(t *testing.T) {
	a := &archivedGroupAPI{base: newFakeAPI(0)}
	p := openArchivedGroupPage(t, a, "w_003", "w_004")
	row := `#archived [data-select-workgroup="g_test"]`
	p.await("the archived workgroup row", `document.querySelector(`+quote(row)+`)`)
	p.await("an enabled archived workgroup row", `!document.querySelector(`+quote(row)+`).disabled`)
	p.eval(`document.getElementById('archived').open = true`, nil)
	p.run(chromedp.Click(row, chromedp.ByQuery))
	p.await("the workgroup view", `location.hash === '#workgroup=g_test' && !document.querySelector('[data-view=workgroup]').hidden`)
	p.awaitText("#workgroup-content .feed", "Both repositories agreed on the shared interface.")
	p.awaitText("#workgroup-content .feed", "Archived workstream w_004 because its pull request merged")
	p.await("members shown as archived", `document.querySelectorAll('#workgroup-content [data-member] [data-field=archived]').length === 2`)
	p.await("the read-only composer", `(() => {
  const form = document.querySelector('#workgroup-content form[data-field=send]');
  return form && (form.hidden || getComputedStyle(form).display === 'none') && form.querySelector('textarea').disabled && form.querySelector('button[type=submit]').disabled;
 })()`)
	p.await("a read-only note", `document.querySelector('#workgroup-content [data-field=read-only]')`)
	p.await("no archived member to ask about", `document.querySelectorAll('#workgroup-content [data-field=about]').length === 0 && document.querySelectorAll('#workgroup-content .member-open:not(:disabled)').length === 0`)
	var text string
	p.eval(`document.body.innerText`, &text)
	if strings.Contains(text, "This workgroup is unavailable.") {
		t.Fatalf("the retained workgroup is shown as unavailable:\n%s", text)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sent != 0 {
		t.Fatalf("the read-only view sent %d messages", a.sent)
	}
}

// A workgroup whose shared feed is gone, or that the status no longer lists, is
// unavailable.
func TestBrowserWorkgroupWithoutItsSharedFeedIsUnavailable(t *testing.T) {
	t.Run("feed gone", func(t *testing.T) {
		a := &archivedGroupAPI{base: newFakeAPI(0), gone: true}
		p := openArchivedGroupPage(t, a, "w_003", "w_004")
		row := `#archived [data-select-workgroup="g_test"]`
		p.await("the archived workgroup row", `document.querySelector(`+quote(row)+`)`)
		p.eval(`document.getElementById('archived').open = true`, nil)
		p.run(chromedp.Click(row, chromedp.ByQuery))
		p.awaitText("#workgroup-content", "This workgroup is unavailable.")
		p.await("no conversation", `!document.querySelector('#workgroup-content .feed')`)
	})
	t.Run("workgroup gone", func(t *testing.T) {
		a := &archivedGroupAPI{base: newFakeAPI(0)}
		server := httptest.NewServer(a.handler("w_003", "w_004"))
		t.Cleanup(server.Close)
		p := openBrowser(t)
		p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/#workgroup=g_missing"))
		p.awaitText("#workgroup-content", "This workgroup is unavailable.")
	})
}
