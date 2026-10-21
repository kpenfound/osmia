package service

import (
	"testing"

	"github.com/chromedp/chromedp"
)

func TestBrowserDeliveredWorkstreamCanCheckForDrift(t *testing.T) {
	t.Parallel()
	f := newPublicationFixtureWith(t, "commit-per-unit", listenWeb)
	f.approve(t, nil)
	result := settleOperation(t, f.s, f.repository, f.stream, f.request(t), f.publisher())
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	must(t, f.repository.Close())
	f.opts.PullRequests = f.pulls
	f.start(t)
	t.Cleanup(func() { f.stop(t) })
	p := openBrowser(t)
	p.Run(chromedp.Navigate("http://" + f.s.WebAddr() + "/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(f.stream)
	p.AwaitText(`#workstream-head [data-field=state]`, "delivered")
	p.Click("#workstream-menu-button")
	p.Await("delivered drift action", `!document.querySelector('[data-workstream-action="rebase"]').hidden`)
	p.Click(`[data-workstream-action="rebase"]`)
	p.AwaitText("#inbox-result", "Drift rebase 1")
	state, err := f.s.sole().repository.Workflow(f.stream, driftRequestSubject)
	must(t, err)
	if state.Value != "requested-1" {
		t.Fatalf("drift request: %+v", state)
	}
}
