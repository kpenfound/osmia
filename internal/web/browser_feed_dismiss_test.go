package web

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
)

type fakeAction struct {
	mu          sync.Mutex
	hold        chan struct{}
	fail        string
	unavailable string
}

func (a *fakeAction) holdResponse() (release func()) {
	ch := make(chan struct{})
	a.mu.Lock()
	a.hold = ch
	a.mu.Unlock()
	return func() { close(ch) }
}

func (a *fakeAction) failNext(message string) {
	a.mu.Lock()
	a.fail, a.unavailable = message, ""
	a.mu.Unlock()
}

func (a *fakeAction) failNextUnavailable(message string) {
	a.mu.Lock()
	a.fail, a.unavailable = "", message
	a.mu.Unlock()
}

func (a *fakeAction) serve(w http.ResponseWriter, r *http.Request, okBody string) {
	io.Copy(io.Discard, r.Body)
	a.mu.Lock()
	hold := a.hold
	fail := a.fail
	unavailable := a.unavailable
	a.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if unavailable != "" {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, fmt.Sprintf(`{"error":{"code":"unavailable","message":%s}}`, quote(unavailable)))
		return
	}
	if fail != "" {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, fmt.Sprintf(`{"error":{"message":%s}}`, quote(fail)))
		return
	}
	writeJSON(w, okBody)
}

func contestedEntry(workstream, unit string) string {
	return fmt.Sprintf(`{"kind":"contested","project":"p_fake","workstream":%s,"number":0,"batch":"","unit":%s,`+
		`"amendment":"","revision":1,"question":"Unit %s is contested: fake","blocked":"Unit %s.",`+
		`"options":["review","revise"],"recommendation":"","quick_reply":"","opened_at":"2026-01-01T00:00:00Z",`+
		`"asked":[],"answer":{"method":"POST","path":"/v1/contested/%s/%s","body":{}}}`,
		quote(workstream), quote(unit), unit, unit, workstream, unit)
}

func ratificationEntry(workstream string) string {
	return fmt.Sprintf(`{"kind":"ratification","project":"p_fake","workstream":%s,"number":0,"batch":"","unit":"",`+
		`"amendment":"","revision":1,"question":"Ratify spec.md revision 1 and plan.json revision 1? Debate ended after round 1: ratify: no objection stands",`+
		`"blocked":"Sealing the spec and plan, and building the workstream.","options":["ratify"],`+
		`"recommendation":"ratify: no objection stands","quick_reply":"","opened_at":"2026-01-01T00:00:00Z",`+
		`"asked":[],"answer":{"method":"POST","path":"/v1/ratify/%s","body":{"spec":1,"plan":1}}}`,
		quote(workstream), workstream)
}

func decisionSelector(kind, workstream, unit string) string {
	return fmt.Sprintf(`[data-decision="%s:p_fake:%s:0:%s:"] `, kind, workstream, unit)
}

func (p *page) click(selector string) {
	p.t.Helper()
	p.run(chromedp.Evaluate(`document.querySelector(`+quote(selector)+`)?.scrollIntoView({block: 'center'})`, nil),
		chromedp.Click(selector, chromedp.ByQuery))
}

func (p *page) typeInto(selector, text string) {
	p.t.Helper()
	p.run(chromedp.SendKeys(selector, text, chromedp.ByQuery))
}

func (p *page) awaitGone(what, selector string) {
	p.t.Helper()
	p.await(what+" to leave the feed", `document.querySelector(`+quote(selector)+`) === null`)
}

func (p *page) awaitVisible(what, selector string) {
	p.t.Helper()
	p.await(what+" in the feed", `document.querySelector(`+quote(selector)+`) !== null`)
}

func newDismissPage(t *testing.T, entries ...string) (*page, *fakeAPI) {
	t.Helper()
	api := newFakeAPI(1)
	api.setInbox(entries...)
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.click(`[data-select="w_000"]`)
	return p, api
}

func TestBrowserFeedDismissesAContestedRulingBeforeTheServerResponds(t *testing.T) {
	t.Parallel()
	p, api := newDismissPage(t, contestedEntry("w_000", "index"))
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)
	release := api.contested.holdResponse()

	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + `[data-decision-choice="review"]`)

	p.awaitGone("the contested unit", card)
	release()
	p.awaitOutcome("Ruled review on unit index.")
}

func TestBrowserFeedDismissesARatificationBeforeTheServerResponds(t *testing.T) {
	t.Parallel()
	p, api := newDismissPage(t, ratificationEntry("w_000"))
	card := decisionSelector("ratification", "w_000", "")
	p.awaitVisible("the ratification packet", card)
	release := api.ratify.holdResponse()

	p.click(card + `[data-decision-choice="ratify"]`)

	p.awaitGone("the ratification packet", card)
	release()
	p.awaitOutcome("the owner ratified the packet.")
}

func TestBrowserFeedDismissedItemStaysHiddenUntilARefreshDropsItThenShowsAgain(t *testing.T) {
	t.Parallel()
	p, api := newDismissPage(t, contestedEntry("w_000", "index"))
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)

	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + `[data-decision-choice="review"]`)
	p.awaitGone("the contested unit", card)
	p.awaitOutcome("Ruled review on unit index.")

	api.pushEvent("inbox")
	p.awaitGone("the contested unit", card)
	p.assertOutcomes("#workstreams", "Ruled review on unit index.")

	hits := api.inboxRequests()
	api.setInbox()
	api.pushEvent("inbox")
	api.inbox.await(t, "the page never fetched the emptied inbox", func(n int) bool { return n > hits })
	p.await("the inbox emptied", `document.querySelectorAll('[data-decision]').length === 0`)
	api.setInbox(contestedEntry("w_000", "index"))
	api.pushEvent("inbox")
	p.awaitVisible("the recontested unit", card)
}

func TestBrowserFeedDismissalExpiresAfter120SecondsIfStillListed(t *testing.T) {
	t.Parallel()
	p, api := newDismissPage(t, contestedEntry("w_000", "index"))
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)

	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + `[data-decision-choice="review"]`)
	p.awaitGone("the contested unit", card)
	p.awaitOutcome("Ruled review on unit index.")

	api.pushEvent("inbox")
	p.awaitGone("the contested unit", card)

	var realNow int64
	p.eval("Date.now()", &realNow)
	p.eval("Date.now = () => "+strconv.FormatInt(realNow+121000, 10), nil)

	p.awaitVisible("the contested unit, come back after its comeback window", card)
}

func TestBrowserFeedFailedSubmissionRestoresTheItemAtOnceWithErrorAndInput(t *testing.T) {
	t.Parallel()
	p, api := newDismissPage(t, contestedEntry("w_000", "index"))
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)
	release := api.contested.holdResponse()
	api.contested.failNext("the reviewer's turn is still running")

	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + `[data-decision-choice="review"]`)
	p.awaitGone("the contested unit", card)

	release()
	p.awaitText("#inbox-result", "the reviewer's turn is still running")
	p.awaitVisible("the contested unit, restored", card)
	var note string
	p.eval(`document.querySelector(`+quote(card+`input[name=note]`)+`).value`, &note)
	if note != "Review it again." {
		t.Fatalf("the restored contested card note: %q", note)
	}
}

func TestBrowserFeedDismissingOneItemLeavesOthersUnaffected(t *testing.T) {
	t.Parallel()
	p, api := newDismissPage(t, contestedEntry("w_000", "index"), ratificationEntry("w_000"))
	contested := decisionSelector("contested", "w_000", "index")
	ratification := decisionSelector("ratification", "w_000", "")
	p.awaitVisible("the contested unit", contested)
	p.awaitVisible("the ratification packet", ratification)
	release := api.contested.holdResponse()

	p.typeInto(contested+`input[name=note]`, "Review it again.")
	p.click(contested + `[data-decision-choice="review"]`)

	p.awaitGone("the contested unit", contested)
	p.awaitText(ratification+"[data-field=options]", "Ratify")
	var gone bool
	p.eval(`document.querySelector(`+quote(ratification)+`) === null`, &gone)
	if gone {
		t.Fatal("dismissing the contested ruling also hid the unrelated ratification packet")
	}

	release()
	p.awaitOutcome("Ruled review on unit index.")
	p.awaitVisible("the ratification packet, still there", ratification)
}
