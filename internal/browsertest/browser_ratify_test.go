package browsertest

import (
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestBrowserRatifyButtonRecordsRatificationAndLeavesGate(t *testing.T) {
	p := openBrowser(t)
	opts, clone := projectFixture(t)
	engine, _, clock, architect, issues := intakefixture.ArchitectExecution(t, opts.Config, clone)
	opts.Reconciliation.Now, opts.Architect, opts.Issues = clock.Now, architect, issues
	withWeb(t, opts, "127.0.0.1:0")
	intakefixture.Upstream(t, clone)
	s, c := start(t, opts)
	intakefixture.AddArchitectProject(t, c, clone, intakefixture.ShedCharter, func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, c, p, text) })
	scenario := &intakefixture.ArchitectScenario{
		Client: func() intakefixture.WorkflowClient { return c }, Engine: engine,
		Repository: func() *trace.Repository { return service.InspectForTest(s).SoleRepository() },
		Changes:    func(t *testing.T) (func(), func()) { return servicetest.ServiceChanges(t, s) },
	}
	scenario.ScriptDebate(1)
	ws := scenario.ShedAs(t, "browser-ratify")
	entries := entriesOf(t, c, intake.InboxRatification)
	if len(entries) != 1 || entries[0].Workstream != ws {
		t.Fatalf("ratification entries: %+v", entries)
	}
	card := decisionCard(entries[0])
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+`[data-decision-choice="ratify"]`, "Ratify")
	p.Click(card + `[data-decision-choice="ratify"]`)
	// The service's outcome joins the workstream's feed as an entry marked as
	// the owner's command outcome, with the time it was accepted; the shared
	// result line shows nothing.
	const outcome = "the owner ratified spec.md revision 1 and plan.json revision 1"
	feed := `#workstreams [data-field=feed] `
	p.AwaitOutcome(outcome)
	p.Await("one outcome entry marked as the owner's command with a time", `(() => {
		const entries = document.querySelectorAll(`+quote(feed+"[data-kind=outcome]")+`);
		return entries.length === 1 && entries[0].dataset.outcome === 'ok' &&
			entries[0].querySelector('[data-ann=role]').textContent === 'Your command' &&
			entries[0].querySelector('[data-field=text]').textContent.startsWith(`+quote(outcome)+`) &&
			entries[0].querySelector('[data-ann=when]').textContent !== '';
	})()`)
	p.Await("no text in the shared result line", `document.querySelector('#inbox-result').textContent === '' && !document.querySelector('#inbox-result').dataset.outcome`)
	p.AwaitGone("the ratified packet", card)
	record := scenario.Ratification(t, ws, 1)
	if record.Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("recorded ratification: %+v", record)
	}
	scenario.AwaitFeature(t, ws, intake.BuildingState)
	// What the service adds to the feed after the ratification is accepted,
	// such as the workstream's transitions as sealing and building proceed,
	// follows the outcome entry; the outcome stays a single entry through the
	// feed's re-reads.
	p.Await("a feed entry the service added after the ratification rendered after the outcome entry", `(() => {
		const items = [...document.querySelectorAll(`+quote(feed+"ol > li")+`)];
		const at = items.findIndex(n => n.dataset.kind === 'outcome');
		return at >= 0 && items.slice(at + 1).some(n => n.dataset.kind === 'transition');
	})()`)
	if texts := p.OutcomeTexts("#workstreams"); len(texts) != 1 {
		t.Fatalf("outcome entries after the feed was read again: %q", texts)
	}
	p.Await("the shared result line still empty", `document.querySelector('#inbox-result').textContent === ''`)
	if entries := entriesOf(t, c, intake.InboxRatification); len(entries) != 0 {
		t.Fatalf("ratification gate remains: %+v", entries)
	}
}
