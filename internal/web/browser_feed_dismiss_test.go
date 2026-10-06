package web

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// soon polls condition until it is true, failing t with message once
// browserTimeout passes rather than waiting on a fixed sleep.
func soon(t *testing.T, message string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(browserTimeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s", message)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fakeAction answers one inbox entry's own decision endpoint: a fixed body
// by default, held open until release is called so a test can see what the
// page does before the server responds, or made to fail so a test can see
// what it does when a submission is refused.
type fakeAction struct {
	mu   sync.Mutex
	hold chan struct{}
	fail string
}

// holdResponse makes every response already in flight, and every one
// requested until release is called, wait for release.
func (a *fakeAction) holdResponse() (release func()) {
	ch := make(chan struct{})
	a.mu.Lock()
	a.hold = ch
	a.mu.Unlock()
	return func() { close(ch) }
}

// failNext makes every request from now on fail with message, instead of
// answering okBody, until cleared.
func (a *fakeAction) failNext(message string) {
	a.mu.Lock()
	a.fail = message
	a.mu.Unlock()
}

func (a *fakeAction) serve(w http.ResponseWriter, r *http.Request, okBody string) {
	io.Copy(io.Discard, r.Body)
	a.mu.Lock()
	hold := a.hold
	fail := a.fail
	a.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if fail != "" {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, fmt.Sprintf(`{"error":{"message":%s}}`, quote(fail)))
		return
	}
	writeJSON(w, okBody)
}

// contestedEntry is the JSON of an open contested-unit decision on unit,
// shaped the way the real inbox answers it.
func contestedEntry(workstream, unit string) string {
	return fmt.Sprintf(`{"kind":"contested","project":"p_fake","workstream":%s,"number":0,"batch":"","unit":%s,`+
		`"amendment":"","revision":1,"question":"Unit %s is contested: fake","blocked":"Unit %s.",`+
		`"options":["review","revise"],"recommendation":"","quick_reply":"","opened_at":"2026-01-01T00:00:00Z",`+
		`"asked":[],"answer":{"method":"POST","path":"/v1/contested/%s/%s","body":{}}}`,
		quote(workstream), quote(unit), unit, unit, workstream, unit)
}

// ratificationEntry is the JSON of an open ratification decision, shaped
// the way the real inbox answers it.
func ratificationEntry(workstream string) string {
	return fmt.Sprintf(`{"kind":"ratification","project":"p_fake","workstream":%s,"number":0,"batch":"","unit":"",`+
		`"amendment":"","revision":1,"question":"Ratify spec.md revision 1 and plan.json revision 1? Debate ended after round 1: ratify: no objection stands",`+
		`"blocked":"Sealing the spec and plan, and building the workstream.","options":["ratify"],`+
		`"recommendation":"ratify: no objection stands","quick_reply":"","opened_at":"2026-01-01T00:00:00Z",`+
		`"asked":[],"answer":{"method":"POST","path":"/v1/ratify/%s","body":{"spec":1,"plan":1}}}`,
		quote(workstream), workstream)
}

// decisionSelector is the page's selector for the card of an inbox entry of
// kind, on workstream, with unit (contested) or amendment (none here).
func decisionSelector(kind, workstream, unit string) string {
	return fmt.Sprintf(`[data-decision="%s:p_fake:%s:0:%s:"] `, kind, workstream, unit)
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

// awaitGone waits until the page shows no element for selector.
func (p *page) awaitGone(what, selector string) {
	p.t.Helper()
	p.await(what+" to leave the feed", `document.querySelector(`+quote(selector)+`) === null`)
}

// awaitVisible waits until the page shows an element for selector.
func (p *page) awaitVisible(what, selector string) {
	p.t.Helper()
	p.await(what+" in the feed", `document.querySelector(`+quote(selector)+`) !== null`)
}

// newDismissPage opens a browser on a fresh fake API serving one workstream
// with the given inbox entries, and waits for the live connection.
func newDismissPage(t *testing.T, entries ...string) (*page, *fakeAPI) {
	t.Helper()
	api := newFakeAPI(1)
	api.setInbox(entries...)
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	return p, api
}

// Submitting a ruling on a contested workstream removes its item from the
// feed at once, before the faked server answers.
func TestBrowserFeedDismissesAContestedRulingBeforeTheServerResponds(t *testing.T) {
	p, api := newDismissPage(t, contestedEntry("w_000", "index"))
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)
	release := api.contested.holdResponse()

	p.choose(card+"select", "review")
	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + "button[type=submit]")

	// The card is gone before the held response is released.
	p.awaitGone("the contested unit", card)
	release()
	p.awaitText("#inbox-result", "Ruled review on unit index.")
}

// Ratifying a proposal removes its item from the feed at once, before the
// faked server answers.
func TestBrowserFeedDismissesARatificationBeforeTheServerResponds(t *testing.T) {
	p, api := newDismissPage(t, ratificationEntry("w_000"))
	card := decisionSelector("ratification", "w_000", "")
	p.awaitVisible("the ratification packet", card)
	release := api.ratify.holdResponse()

	p.choose(card+"select", "ratify")
	p.click(card + "button[type=submit]")

	p.awaitGone("the ratification packet", card)
	release()
	p.awaitText("#inbox-result", "the owner ratified the packet.")
}

// Once a submission is acknowledged, the dismissed item stays hidden
// through refreshes that still list it, and stops being hidden once a
// refreshed state no longer lists it, at which point the entry, if relisted
// under the same identity, shows again rather than staying hidden.
func TestBrowserFeedDismissedItemStaysHiddenUntilARefreshDropsItThenShowsAgain(t *testing.T) {
	p, api := newDismissPage(t, contestedEntry("w_000", "index"))
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)

	p.choose(card+"select", "review")
	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + "button[type=submit]")
	p.awaitGone("the contested unit", card)
	p.awaitText("#inbox-result", "Ruled review on unit index.")

	// The backend has not actually processed the ruling yet: a refresh
	// that still lists the same entry leaves it hidden.
	api.pushEvent("inbox")
	p.awaitText("#inbox-result", "Ruled review on unit index.")
	p.awaitGone("the contested unit", card)

	// Once a refreshed inbox no longer lists it, the dismissal clears. If
	// the unit is then contested again under the same identity, the page
	// shows it fresh, rather than treating it as still dismissed. The
	// card already reads as gone before this point, so that alone would
	// not prove the page fetched the emptied inbox: relisting before it
	// does would let a slow or coalesced refresh skip straight from
	// "still listed" to "relisted" without ever seeing it empty, leaving
	// the dismissal in place. Wait for the fake API to have actually
	// answered /inbox with the empty state before relisting.
	hits := api.inboxRequests()
	api.setInbox()
	api.pushEvent("inbox")
	soon(t, "the page never fetched the emptied inbox", func() bool { return api.inboxRequests() > hits })
	p.await("the inbox emptied", `document.querySelectorAll('[data-decision]').length === 0`)
	api.setInbox(contestedEntry("w_000", "index"))
	api.pushEvent("inbox")
	p.awaitVisible("the recontested unit", card)
}

// If the server is still listing a dismissed item 120 seconds after
// acknowledging its submission, the item comes back as actionable, so it
// can never be hidden for good; a fake clock makes this testable without
// waiting on it in real time.
func TestBrowserFeedDismissalExpiresAfter120SecondsIfStillListed(t *testing.T) {
	p, api := newDismissPage(t, contestedEntry("w_000", "index"))
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)

	p.choose(card+"select", "review")
	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + "button[type=submit]")
	p.awaitGone("the contested unit", card)
	p.awaitText("#inbox-result", "Ruled review on unit index.")

	// The backend is still listing the same entry, so without the 120s
	// bound it would stay hidden forever.
	api.pushEvent("inbox")
	p.awaitGone("the contested unit", card)

	var realNow int64
	p.eval("Date.now()", &realNow)
	p.eval("Date.now = () => "+strconv.FormatInt(realNow+121000, 10), nil)

	p.awaitVisible("the contested unit, come back after its comeback window", card)
}

// If a submission fails, the item comes back at once, with the refusal
// shown and the owner's input kept.
func TestBrowserFeedFailedSubmissionRestoresTheItemAtOnceWithErrorAndInput(t *testing.T) {
	p, api := newDismissPage(t, contestedEntry("w_000", "index"))
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)
	release := api.contested.holdResponse()
	api.contested.failNext("the reviewer's turn is still running")

	p.choose(card+"select", "review")
	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + "button[type=submit]")
	p.awaitGone("the contested unit", card)

	release()
	p.awaitText("#inbox-result", "the reviewer's turn is still running")
	p.awaitVisible("the contested unit, restored", card)
	var decision, note string
	p.eval(`document.querySelector(`+quote(card+"select")+`).value`, &decision)
	p.eval(`document.querySelector(`+quote(card+`input[name=note]`)+`).value`, &note)
	if decision != "review" || note != "Review it again." {
		t.Fatalf("the restored contested card: decision %q, note %q", decision, note)
	}
}

// Submitting an action on one feed item leaves every other item in the feed
// visible and unchanged.
func TestBrowserFeedDismissingOneItemLeavesOthersUnaffected(t *testing.T) {
	p, api := newDismissPage(t, contestedEntry("w_000", "index"), ratificationEntry("w_000"))
	contested := decisionSelector("contested", "w_000", "index")
	ratification := decisionSelector("ratification", "w_000", "")
	p.awaitVisible("the contested unit", contested)
	p.awaitVisible("the ratification packet", ratification)
	release := api.contested.holdResponse()

	p.choose(contested+"select", "review")
	p.typeInto(contested+`input[name=note]`, "Review it again.")
	p.click(contested + "button[type=submit]")

	p.awaitGone("the contested unit", contested)
	// The ratification card is untouched: still visible, with its options
	// unchanged.
	p.awaitText(ratification+"[data-field=options]", "Options: ratify")
	var gone bool
	p.eval(`document.querySelector(`+quote(ratification)+`) === null`, &gone)
	if gone {
		t.Fatal("dismissing the contested ruling also hid the unrelated ratification packet")
	}

	release()
	p.awaitText("#inbox-result", "Ruled review on unit index.")
	p.awaitVisible("the ratification packet, still there", ratification)
}
