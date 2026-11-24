package executionjourney

import (
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
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
	fixturekit.Must(t, f.repository.Close())
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
	p.AwaitOutcome("Drift rebase 1")
	state, err := f.s.sole().repository.Workflow(f.stream, lifecycle.DriftRequestSubject)
	fixturekit.Must(t, err)
	if state.Value != "requested-1" {
		t.Fatalf("drift request: %+v", state)
	}
}
