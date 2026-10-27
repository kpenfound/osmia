package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chromedp/chromedp"
)

func TestBrowserRatifySendsDisplayedProjectSpecRevision(t *testing.T) {
	api := newFakeAPI(1)
	api.setInbox(ratificationEntry("w_000"))
	requests := make(chan map[string]int, 1)
	base := api.handler()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/packet/w_000":
			writeJSON(w, `{"packet":{"revision":{"spec":1,"plan":1},"dissent":[]},"project_spec_revision":7,"project_spec":{"behavior":"Uploads resume.","slices":[]}}`)
		case "/v1/ratify/w_000":
			var req map[string]int
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			requests <- req
			writeJSON(w, `{"detail":"the owner ratified the packet."}`)
		default:
			base.ServeHTTP(w, r)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.Navigate(server.URL + "/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.click(`[data-select="w_000"]`)
	card := decisionSelector("ratification", "w_000", "")
	p.awaitText(card+`[data-field=project-spec]`, "Workgroup spec revision 7")
	p.choose(card+"select", "ratify")
	p.click(card + "button[type=submit]")
	p.awaitText("#inbox-result", "the owner ratified the packet.")
	req := <-requests
	if req["spec"] != 1 || req["plan"] != 1 || req["project_spec_revision"] != 7 {
		t.Fatalf("ratify did not send displayed revision: %+v", req)
	}
}
