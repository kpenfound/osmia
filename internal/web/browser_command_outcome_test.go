package web

import (
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
)

// outcomeOf reads the data-outcome attribute #inbox-result currently shows.
func (p *page) outcomeOf(selector string) string {
	p.t.Helper()
	var outcome string
	p.eval(`document.querySelector(`+quote(selector)+`)?.dataset.outcome ?? ''`, &outcome)
	return outcome
}

// Deciding a ratification shows the three outcomes the owner's command can
// have: accepted and durably recorded ('ok'), temporarily unavailable with
// the blocking operation named ('unavailable'), and rejected with a reason
// ('error'). All three leave the card actionable again with the owner's
// choice kept, since none of them is the card's own terminal decision.
func TestBrowserPageShowsAcceptedUnavailableAndRejectedCommandOutcomes(t *testing.T) {
	api := newFakeAPI(1)
	api.setInbox(ratificationEntry("w_000"))
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.click(`[data-select="w_000"]`)

	card := decisionSelector("ratification", "w_000", "")
	p.awaitVisible("the ratification packet", card)

	// Temporarily unavailable: the sealing controller is busy with another
	// operation. The card comes back so the owner can try again, and the
	// result names the blocking operation. The response is held open so the
	// card's "gone" state, between the optimistic dismiss and the restore
	// that follows the failure, is actually observable: answering at once
	// would let dismiss and restore both happen before awaitGone's next
	// poll, so the window could close before the test ever sees it.
	release := sync.OnceFunc(api.ratify.holdResponse())
	t.Cleanup(release)
	api.ratify.failNextUnavailable("sealing workstream w_000 is already in progress")
	p.choose(card+"select", "ratify")
	p.click(card + "button[type=submit]")
	p.awaitGone("the ratification packet", card)
	release()
	p.awaitText("#inbox-result", "sealing workstream w_000 is already in progress")
	if outcome := p.outcomeOf("#inbox-result"); outcome != "unavailable" {
		t.Fatalf("unavailable outcome: data-outcome=%q", outcome)
	}
	p.awaitVisible("the ratification packet, restored after unavailable", card)

	// Rejected: the owner's view is stale. The card comes back the same
	// way, but the outcome is distinct from unavailable. Held open for the
	// same reason as above.
	release = sync.OnceFunc(api.ratify.holdResponse())
	t.Cleanup(release)
	api.ratify.failNext("spec.md revision 2 is ratified already, read it with osmia status")
	p.choose(card+"select", "ratify")
	p.click(card + "button[type=submit]")
	p.awaitGone("the ratification packet", card)
	release()
	p.awaitText("#inbox-result", "spec.md revision 2 is ratified already")
	if outcome := p.outcomeOf("#inbox-result"); outcome != "error" {
		t.Fatalf("rejected outcome: data-outcome=%q", outcome)
	}
	p.awaitVisible("the ratification packet, restored after rejection", card)

	// Accepted: a fresh submission succeeds and dismisses the card for
	// good. Success never restores the card (app.js's submit calls
	// acknowledge, not restore, on success), so there is no "gone, then
	// back" window to race here; holding the response is not needed to
	// observe the dismissal, only to see it happen before the server
	// answers, as the already-passing dismiss tests do.
	release = sync.OnceFunc(api.ratify.holdResponse())
	t.Cleanup(release)
	api.ratify.failNext("")
	p.choose(card+"select", "ratify")
	p.click(card + "button[type=submit]")
	p.awaitGone("the ratification packet", card)
	release()
	p.awaitText("#inbox-result", "the owner ratified the packet.")
	if outcome := p.outcomeOf("#inbox-result"); outcome != "ok" {
		t.Fatalf("accepted outcome: data-outcome=%q", outcome)
	}
}
