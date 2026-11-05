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
	p.AwaitText("#inbox-result", "the owner ratified spec.md revision 1 and plan.json revision 1")
	p.AwaitGone("the ratified packet", card)
	record := scenario.Ratification(t, ws, 1)
	if record.Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("recorded ratification: %+v", record)
	}
	scenario.AwaitFeature(t, ws, intake.BuildingState)
	if entries := entriesOf(t, c, intake.InboxRatification); len(entries) != 0 {
		t.Fatalf("ratification gate remains: %+v", entries)
	}
}
