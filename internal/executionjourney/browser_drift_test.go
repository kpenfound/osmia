package executionjourney

import (
	"testing"

	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/chromedp/chromedp"
)

func TestBrowserDeliveredWorkstreamCanCheckForDrift(t *testing.T) {
	t.Parallel()
	f := journey.NewPublicationFixtureWith(t, "commit-per-unit", journey.ListenWeb)
	f.Approve(t, nil)
	result := journey.SettleOperation(t, f.S, f.Repository, f.Stream, f.Request(t), f.Publisher())
	if result.Outcome != "succeeded" {
		t.Fatal(result)
	}
	fixturekit.Must(t, f.Repository.Close())
	f.Opts.PullRequests = f.Pulls
	f.Start(t)
	t.Cleanup(func() { f.Stop(t) })
	p := openBrowser(t)
	p.Run(chromedp.Navigate("http://" + f.S.WebAddr() + "/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(f.Stream)
	p.AwaitText(`#workstream-head [data-field=state]`, "delivered")
	p.Click("#workstream-menu-button")
	p.Await("delivered drift action", `!document.querySelector('[data-workstream-action="rebase"]').hidden`)
	p.Click(`[data-workstream-action="rebase"]`)
	p.AwaitOutcome("Drift rebase 1")
	state, err := f.S.Sole().Repository.Workflow(f.Stream, lifecycle.DriftRequestSubject)
	fixturekit.Must(t, err)
	if state.Value != "requested-1" {
		t.Fatalf("drift request: %+v", state)
	}
}
