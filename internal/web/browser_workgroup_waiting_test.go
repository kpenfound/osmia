package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

const waitingHeading = `#workgroup-content h3.needs-title`

func openWorkgroupWaitingPage(t *testing.T, entries ...string) (*page, *fakeAPI) {
	t.Helper()
	api := newFakeAPI(0)
	api.setInbox(entries...)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		var rows []string
		for _, id := range []string{"w_003", "w_004"} {
			rows = append(rows, fmt.Sprintf(`{"workstream":%q,"project":"p_fake","workspaces":"git","state":"building","display_state":"building",`+
				`"workgroup":{"id":"g_test","members":["w_003","w_004"],"edges":[]},`+
				`"units":[],"advisories":[],"open_questions":0,"gates":[],"context_mode":"file","drift":null,"status":null,"agents":[]}`, id))
		}
		writeJSON(w, `{"workstreams":[`+strings.Join(rows, ",")+`],"profiles":{},"daily_budget":null,"capacity":null,`+
			`"provider_usage":null,"diagnostics":[],"workspaces":{"setting":"git","backend":"git"}}`)
	})
	mux.HandleFunc("/v1/workgroups/g_test/feed", handleJSON(`{"workgroup":"g_test","members":[],"entries":[]}`))
	mux.Handle("/", api.handler())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	row := `[data-select-workgroup="g_test"]`
	p.awaitVisible("the workgroup row", row)
	p.click(row)
	p.awaitVisible("the workgroup composer", `#workgroup-content form[data-field=send]`)
	api.inbox.await(t, "the page never read the inbox", func(n int) bool { return n > 0 })
	return p, api
}

func refreshInbox(t *testing.T, api *fakeAPI) {
	t.Helper()
	hits := api.inboxRequests()
	api.pushEvent("inbox")
	api.inbox.await(t, "the page never read the inbox again", func(n int) bool { return n > hits })
}

func TestBrowserWorkgroupWaitingHeadingTracksTheCardsShown(t *testing.T) {
	t.Parallel()
	p, api := openWorkgroupWaitingPage(t)
	card := decisionSelector("contested", "w_003", "index")
	p.await("the decision area with no heading and no card", `(() => {
  const area = document.querySelector('#workgroup-content #inbox-result')?.parentElement;
  return !!area && !document.querySelector(`+quote(waitingHeading)+`) && document.querySelectorAll('#workgroup-content [data-decision]').length === 0;
 })()`)

	api.setInbox(contestedEntry("w_003", "index"))
	refreshInbox(t, api)
	p.awaitVisible("the contested unit", card)
	p.await("the heading above the card", `(() => {
  const heading = document.querySelector(`+quote(waitingHeading)+`), card = document.querySelector(`+quote(card)+`);
  return !!heading && heading.textContent === 'Waiting for you' &&
   !!(heading.compareDocumentPosition(card) & Node.DOCUMENT_POSITION_FOLLOWING) && heading.parentElement === card.parentElement;
 })()`)

	release := api.contested.holdResponse()
	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + `[data-decision-choice="review"]`)
	p.awaitGone("the contested unit", card)
	p.awaitGone("the waiting heading", waitingHeading)
	p.awaitText("#workgroup-content #inbox-result", "Sending…")
	release()
	p.awaitOutcome("Ruled review on unit index.")
	refreshInbox(t, api)
	p.awaitGone("the contested unit", card)
	p.awaitGone("the waiting heading", waitingHeading)

	api.setInbox()
	refreshInbox(t, api)
	api.setInbox(ratificationEntry("w_004"))
	refreshInbox(t, api)
	ratification := decisionSelector("ratification", "w_004", "")
	p.awaitVisible("the ratification packet", ratification)
	p.awaitVisible("the waiting heading", waitingHeading)
	api.setInbox()
	refreshInbox(t, api)
	p.awaitGone("the ratification packet", ratification)
	p.awaitGone("the waiting heading", waitingHeading)
}

func TestBrowserWorkgroupWaitingHeadingReturnsWithARestoredCard(t *testing.T) {
	t.Parallel()
	p, api := openWorkgroupWaitingPage(t, contestedEntry("w_003", "index"))
	card := decisionSelector("contested", "w_003", "index")
	p.awaitVisible("the contested unit", card)
	p.awaitVisible("the waiting heading", waitingHeading)
	release := api.contested.holdResponse()
	api.contested.failNext("the reviewer's turn is still running")

	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + `[data-decision-choice="review"]`)
	p.awaitGone("the contested unit", card)
	p.awaitGone("the waiting heading", waitingHeading)

	release()
	p.awaitText("#workgroup-content #inbox-result", "the reviewer's turn is still running")
	p.awaitVisible("the contested unit, restored", card)
	p.awaitVisible("the waiting heading, restored", waitingHeading)
}
