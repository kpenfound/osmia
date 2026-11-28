package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chromedp/chromedp"
)

func TestBrowserCapacityViewShowsTheManagerPoolNextToTheOthers(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(0)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"workstreams":[],"profiles":{},"daily_budget":null,"capacity":{"per_workstream":2,"roles":[`+
			`{"role":"mason","used":0,"limit":4,"waiting":[]},`+
			`{"role":"checks","used":0,"limit":2,"waiting":[]},`+
			`{"role":"manager","used":3,"limit":3,"waiting":[`+
			`{"project":"p_fake","workstream":"w_007","agent":"manager","turn":"event-1","reason":"capacity"},`+
			`{"project":"p_fake","workstream":"w_008","agent":"manager","turn":"event-2","reason":"capacity"}]}]},`+
			`"provider_usage":null,"diagnostics":[],"workspaces":{"setting":"git","backend":"git"}}`)
	})
	mux.Handle("/", api.handler())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.awaitText(`#capacity [data-role=mason] [data-field=slots]`, "0 / 4 slots")
	p.awaitText(`#capacity [data-role=manager] [data-field=slots]`, "3 / 3 slots, 2 queued")
	p.awaitText(`#capacity [data-role=manager]`, "w_007")
	p.awaitText(`#capacity [data-role=manager]`, "w_008")
	p.await("manager after checks", `(() => {
  const roles = [...document.querySelectorAll('#capacity [data-role]')].map((r) => r.dataset.role);
  return roles.join(',') === 'mason,checks,manager';
 })()`)
	p.await("the manager meter is full", `document.querySelector('[data-meter=manager]').dataset.full === 'true'`)
}
