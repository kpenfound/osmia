package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

const clientsStatus = `{"workstreams":[],"profiles":{},"daily_budget":null,"capacity":{"per_workstream":2,"roles":[` +
	`{"role":"mason","used":1,"limit":4,"waiting":[{"workstream":"w_007","agent":"mason-1","turn":"t-wait","reason":"client","client":"nNODE1"}]}],"clients":[` +
	`{"id":"local","name":"local","local":true,"unbounded":true,"used":1,"draining":false,"reported":true,"report_current":true,` +
	`"profiles":[{"profile":"default","runnable":true}],"skipped":[],"failures":[],` +
	`"turns":[{"turn":"t-local","workstream":"w_007","role":"mason","unit":"parser"}]},` +
	`{"id":"nNODE1","name":"laptop","used":2,"slots":2,"draining":false,"idle":"full","idle_reason":"Every slot is in use; it takes a turn when one ends.",` +
	`"reported":true,"report_current":true,"lease":{"health":"healthy","connected_at":"2026-11-25T09:00:00Z","last_seen":"2026-11-25T09:00:05Z","ends_at":"2026-11-25T09:02:05Z"},` +
	`"profiles":[{"profile":"default","runnable":true},{"profile":"backup","runnable":false,"problems":[{"check":"codex --version","detail":"not found","fix":"install the codex CLI"}]}],` +
	`"skipped":[{"role":"mason","main":"default","profile":"backup"}],` +
	`"turns":[{"turn":"t-remote-1","workstream":"w_007","role":"mason","unit":"lexer"},{"turn":"t-remote-2","workstream":"w_008","role":"reviewer"}],` +
	`"failures":[{"turn":"t-failed","workstream":"w_007","role":"mason","profile":"default","failure":"the session failed: not logged in","class":"infrastructure","at":"2026-11-25T08:50:00Z"}]},` +
	`{"id":"nNODE2","name":"desktop","used":0,"slots":4,"draining":true,"idle":"draining","idle_reason":"It is draining: it takes no new turns and keeps its lease while its running turns finish.",` +
	`"reported":true,"report_current":true,"lease":{"health":"late","connected_at":"2026-11-25T08:00:00Z","last_seen":"2026-11-25T08:59:00Z","ends_at":"2026-11-25T09:01:00Z"},` +
	`"profiles":[{"profile":"default","runnable":true}],"skipped":[],"turns":[],"failures":[]},` +
	`{"id":"nNODE3","name":"server-rack","used":0,"slots":1,"draining":false,"idle":"no-profile","idle_reason":"It can run no profile. Missing: claude --version: install the claude CLI.",` +
	`"reported":true,"report_current":true,"lease":{"health":"healthy","connected_at":"2026-11-25T09:00:00Z","last_seen":"2026-11-25T09:00:05Z","ends_at":"2026-11-25T09:02:05Z"},` +
	`"profiles":[{"profile":"default","runnable":false,"problems":[{"check":"claude --version","detail":"executable not found","fix":"install the claude CLI"}]}],` +
	`"skipped":[],"turns":[],"failures":[]}]},` +
	`"provider_usage":null,"diagnostics":[],"workspaces":{"setting":"git","backend":"git"}}`

func openClientsPage(t *testing.T, width, height int64) (*page, *fakeAPI, *counter) {
	t.Helper()
	api := newFakeAPI(0)
	reads := &counter{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		reads.add()
		writeJSON(w, strings.Replace(clientsStatus, `"per_workstream":2`, fmt.Sprintf(`"per_workstream":%d`, 1+reads.load()), 1))
	})
	mux.Handle("/", api.handler())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(width, height), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	return p, api, reads
}

func TestBrowserCapacityViewListsEachClientWithItsStateProfilesTurnsAndFailures(t *testing.T) {
	t.Parallel()
	p, _, _ := openClientsPage(t, 1280, 900)
	p.await("the client list", `document.querySelectorAll('#capacity [data-field=clients] [data-client]').length >= 4`)
	p.await("the local client first and the rest in order", `(() => {
  const ids = [...document.querySelectorAll('#capacity .client')].map((c) => c.dataset.client);
  return ids.join(',') === 'local,nNODE1,nNODE2,nNODE3';
 })()`)

	local := `#capacity .client[data-client=local]`
	p.awaitText(local+` [data-field=name]`, "Local client")
	p.awaitText(local+` [data-field=slots]`, "1 active, no local limit")
	p.awaitText(local+` [data-field=identity]`, "The server's own machine")
	p.awaitText(local+` [data-field=turn]`, "t-local")
	p.await("no lease on the local client", `!document.querySelector(`+quote(local+` [data-field=lease]`)+`)`)

	laptop := `#capacity .client[data-client=nNODE1]`
	p.awaitText(laptop+` [data-field=name]`, "laptop")
	p.awaitText(laptop+` [data-field=identity]`, "nNODE1")
	p.awaitText(laptop+` [data-field=slots]`, "2 / 2 slots")
	p.awaitText(laptop+` [data-field=lease]`, "Lease healthy")
	p.awaitText(laptop+` [data-field=idle]`, "Every slot is in use")
	p.awaitText(laptop+` [data-field=can-run]`, "default")
	p.awaitText(laptop+` [data-field=cannot-run][data-profile=backup]`, "codex --version")
	p.awaitText(laptop+` [data-field=cannot-run][data-profile=backup] [data-field=fix]`, "install the codex CLI")
	p.awaitText(laptop+` [data-field=skipped]`, "Skips fallback backup for mason")
	p.await("both running turns", `document.querySelectorAll(`+quote(laptop+` [data-field=turn]`)+`).length === 2`)
	p.awaitText(laptop+` [data-field=failure][data-turn=t-failed]`, "Failed")
	p.awaitText(laptop+` [data-field=failure][data-turn=t-failed] [data-field=failure-text]`, "not logged in")

	desktop := `#capacity .client[data-client=nNODE2]`
	p.await("a draining client", `document.querySelector(`+quote(desktop)+`).dataset.draining === 'true'`)
	p.awaitText(desktop+` [data-field=draining]`, "Draining")
	p.awaitText(desktop+` [data-field=lease]`, "Lease late")

	rack := `#capacity .client[data-client=nNODE3]`
	p.awaitText(rack+` [data-field=idle]`, "can run no profile")
	p.awaitText(rack+` [data-field=idle]`, "install the claude CLI")
	p.await("the profile list open for a client that can run nothing", `document.querySelector(`+quote(rack+` details[data-field=profiles]`)+`).open`)
	p.awaitText(rack+` [data-field=cannot-run]`, "executable not found")

	p.await("the wait names the client it waits for", `(() => {
  const w = document.querySelector('#capacity [data-role=mason] [data-field=waiting]');
  return !!w && w.textContent.includes('its client has no free slot') && w.querySelector('[data-field=waiting-client]').textContent === 'nNODE1';
 })()`)
}

func TestBrowserClientProfileListsStayOpenWhenTheStatusIsReadAgain(t *testing.T) {
	t.Parallel()
	p, api, reads := openClientsPage(t, 1280, 900)
	laptop := `#capacity .client[data-client=nNODE1]`
	list := quote(laptop + ` details[data-field=profiles]`)
	p.await("the laptop's profile list folded", `!!document.querySelector(`+list+`) && !document.querySelector(`+list+`).open`)
	p.run(chromedp.Evaluate(`document.querySelector(`+list+`).querySelector('summary').click()`, nil))
	p.await("the list open", `document.querySelector(`+list+`).open`)
	before := reads.load()
	text := `document.querySelector('#capacity [data-field=per-workstream]').textContent`
	var shown string
	p.eval(text, &shown)
	api.pushEvent("capacity")
	reads.await(t, "another read of the status", func(n int) bool { return n > before })
	p.await("the capacity view drawn again", text+` !== `+quote(shown))
	p.await("the profile list still open", `document.querySelector(`+list+`).open`)
}

func TestBrowserCapacityPopoverWithClientsFitsAPhone(t *testing.T) {
	t.Parallel()
	p, _, _ := openClientsPage(t, 390, 800)
	p.await("the client list", `document.querySelectorAll('#capacity .client').length === 4`)
	p.run(chromedp.Evaluate(`document.getElementById('capacity-popover').showPopover()`, nil))
	p.await("a popover that fits the screen and does not scroll sideways", `(() => {
  const pop = document.getElementById('capacity-popover');
  const box = pop.getBoundingClientRect();
  return box.left >= 0 && box.right <= window.innerWidth && pop.scrollWidth <= pop.clientWidth;
 })()`)
}
