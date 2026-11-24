package web

import (
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
)

func (p *page) outcomeOf(selector string) string {
	p.t.Helper()
	var outcome string
	p.eval(`document.querySelector(`+quote(selector)+`)?.dataset.outcome ?? ''`, &outcome)
	return outcome
}

type ratifyOutcomePage struct {
	p    *page
	api  *fakeAPI
	card string
}

func openRatifyOutcomePage(t *testing.T) ratifyOutcomePage {
	t.Helper()
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
	return ratifyOutcomePage{p: p, api: api, card: card}
}

func (o ratifyOutcomePage) ratify(t *testing.T) {
	t.Helper()
	release := sync.OnceFunc(o.api.ratify.holdResponse())
	t.Cleanup(release)
	o.p.click(o.card + `[data-decision-choice="ratify"]`)
	o.p.awaitGone("the ratification packet", o.card)
	release()
}

func TestBrowserPageShowsAnUnavailableCommandOutcome(t *testing.T) {
	t.Parallel()
	o := openRatifyOutcomePage(t)
	o.api.ratify.failNextUnavailable("sealing workstream w_000 is already in progress")
	o.ratify(t)
	o.p.awaitText("#inbox-result", "sealing workstream w_000 is already in progress")
	if outcome := o.p.outcomeOf("#inbox-result"); outcome != "unavailable" {
		t.Fatalf("unavailable outcome: data-outcome=%q", outcome)
	}
}

func TestBrowserPageRestoresTheCardAfterAnUnavailableCommand(t *testing.T) {
	t.Parallel()
	o := openRatifyOutcomePage(t)
	o.api.ratify.failNextUnavailable("sealing workstream w_000 is already in progress")
	o.ratify(t)
	o.p.awaitVisible("the ratification packet, restored after unavailable", o.card)
	o.p.assertNoOutcomes("#workstreams")
}

func TestBrowserPageShowsARejectedCommandOutcome(t *testing.T) {
	t.Parallel()
	o := openRatifyOutcomePage(t)
	o.api.ratify.failNext("spec.md revision 2 is ratified already, read it with osmia status")
	o.ratify(t)
	o.p.awaitText("#inbox-result", "spec.md revision 2 is ratified already")
	if outcome := o.p.outcomeOf("#inbox-result"); outcome != "error" {
		t.Fatalf("rejected outcome: data-outcome=%q", outcome)
	}
}

func TestBrowserPageRestoresTheCardAfterARejectedCommand(t *testing.T) {
	t.Parallel()
	o := openRatifyOutcomePage(t)
	o.api.ratify.failNext("spec.md revision 2 is ratified already, read it with osmia status")
	o.ratify(t)
	o.p.awaitVisible("the ratification packet, restored after rejection", o.card)
	o.p.assertNoOutcomes("#workstreams")
}

func TestBrowserPageShowsAnAcceptedCommandOutcomeInTheFeed(t *testing.T) {
	t.Parallel()
	o := openRatifyOutcomePage(t)
	o.api.ratify.failNext("")
	o.ratify(t)
	o.p.awaitOutcome("the owner ratified the packet.")
	o.p.assertOutcomes("#workstreams", "the owner ratified the packet.")
}

func TestBrowserAcceptedCommandLeavesNoOutcomeOnTheSharedResultLine(t *testing.T) {
	t.Parallel()
	o := openRatifyOutcomePage(t)
	o.api.ratify.failNext("")
	o.ratify(t)
	o.p.awaitOutcome("the owner ratified the packet.")
	if outcome := o.p.outcomeOf("#inbox-result"); outcome != "" {
		t.Fatalf("accepted outcome left data-outcome=%q on the shared result line", outcome)
	}
}
