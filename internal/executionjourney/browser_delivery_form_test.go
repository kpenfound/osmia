package executionjourney

import (
	chromedp "github.com/chromedp/chromedp"
	testing "testing"
)

// The delivery card's drafted description and commit messages clear back to
// blank as soon as the approval they carry is recorded.
func TestBrowserPageResetsTheDeliveryFormAfterSuccess(t *testing.T) {
	p := openBrowser(t)
	f, ws, repository, _ := deliveryFixtureWith(t, listenWeb)
	must(t, repository.Close())
	f.start(t)
	t.Cleanup(func() { f.stop(t) })
	entries := entriesOf(t, f.c, InboxDelivery)
	if len(entries) != 1 {
		t.Fatalf("delivery entries %+v", entries)
	}
	card := decisionCard(entries[0])
	description := card + `textarea[name="description"]`

	p.Run(chromedp.Navigate("http://" + f.s.WebAddr() + "/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Delivery")
	p.Await("the drafted description", `document.querySelector(`+quote(description)+`).value !== ''`)
	p.Click(card + "button[type=submit]")
	p.AwaitText("#inbox-result", "Approved the delivery of final review 1")
	// Approving delivers the workstream, so the card may also leave the
	// inbox before the test next reads it, taking the drafted description
	// with it; either outcome leaves no stale description showing.
	p.AwaitCleared(description)
}
