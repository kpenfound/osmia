package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// waitingHeading is the "Waiting for you" heading inside the workgroup view.
const waitingHeading = `#workgroup-content h3.needs-title`

// openWorkgroupWaitingPage opens a browser on a fake API serving the live
// workgroup g_test of two unarchived members, with the given inbox entries,
// and shows the workgroup view once its composer is there and the page has
// read the inbox.
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
	soon(t, "the page never read the inbox", func() bool { return api.inboxRequests() > 0 })
	return p, api
}

// refreshInbox makes the page read the inbox again and waits until the fake
// API has answered that read.
func refreshInbox(t *testing.T, api *fakeAPI) {
	t.Helper()
	hits := api.inboxRequests()
	api.pushEvent("inbox")
	soon(t, "the page never read the inbox again", func() bool { return api.inboxRequests() > hits })
}

// The workgroup view places the "Waiting for you" heading only while a card
// waits under it.
func TestBrowserWorkgroupWaitingHeadingTracksTheCardsShown(t *testing.T) {
	t.Parallel()
	p, api := openWorkgroupWaitingPage(t)
	card := decisionSelector("contested", "w_003", "index")
	p.await("the decision area with no heading and no card", `(() => {
  const area = document.querySelector('#workgroup-content #inbox-result')?.parentElement;
  return !!area && !document.querySelector(`+quote(waitingHeading)+`) && document.querySelectorAll('#workgroup-content [data-decision]').length === 0;
 })()`)

	// An entry arriving brings the heading, above its card.
	api.setInbox(contestedEntry("w_003", "index"))
	refreshInbox(t, api)
	p.awaitVisible("the contested unit", card)
	p.await("the heading above the card", `(() => {
  const heading = document.querySelector(`+quote(waitingHeading)+`), card = document.querySelector(`+quote(card)+`);
  return !!heading && heading.textContent === 'Waiting for you' &&
   !!(heading.compareDocumentPosition(card) & Node.DOCUMENT_POSITION_FOLLOWING) && heading.parentElement === card.parentElement;
 })()`)

	// Answering the last card dismisses it at once: the heading goes with it
	// while the inbox still lists the entry, and the result line stays.
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

	// Once the inbox stops listing the entry, a new one brings the heading
	// back, and the heading goes again when the inbox drops it.
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

// A workgroup whose only listed entry is dismissed in the page shows no
// heading, and the heading returns with the card when its submission fails.
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
