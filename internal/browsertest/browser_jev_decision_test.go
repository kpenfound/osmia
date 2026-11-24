package browsertest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestBrowserDoesNotLabelAJevGateDecisionAsADeparture(t *testing.T) {
	p := openBrowser(t)
	provider := &jevfixture.Provider{Confidence: 0.97}
	key := jevfixture.Key(t, true)
	opts, clone := projectFixture(t)
	engine, _, clock, architect, issues := intakefixture.ArchitectExecution(t, opts.Config, clone)
	opts.Reconciliation.Now, opts.Architect, opts.Issues = clock.Now, architect, issues
	opts.JevProvider = provider.Factory()
	withWeb(t, opts, "127.0.0.1:0")
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString(jevfixture.Config(key))
	must(t, err)
	must(t, configFile.Close())
	intakefixture.Upstream(t, clone)
	s, c := start(t, opts)
	intakefixture.AddArchitectProject(t, c, clone, intakefixture.ShedCharter, func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, c, p, text) })
	scenario := &intakefixture.ArchitectScenario{
		Client: func() intakefixture.WorkflowClient { return c }, Engine: engine,
		Repository: func() *trace.Repository { return service.InspectForTest(s).SoleRepository() },
		Changes:    func(t *testing.T) (func(), func()) { return servicetest.ServiceChanges(t, s) },
	}
	scenario.ScriptDebate(1)
	ws := scenario.HandInSkipping(t, "browser-jev")
	scenario.AwaitFeature(t, ws, intake.BuildingState)
	docs, err := trace.Read[trace.Document](scenario.Repository(), ws)
	must(t, err)
	var record shed.Ratification
	found := 0
	for _, d := range docs {
		if d.ID == shed.RatificationDocumentID(1) {
			found++
			record, err = shed.ParseRatification([]byte(d.Content))
			must(t, err)
			if d.Actor != trace.JevActor {
				t.Fatalf("the ratification is recorded by %+v", d.Actor)
			}
		}
	}
	if found != 1 {
		t.Fatalf("%d ratification records", found)
	}
	if record.DecidedBy != trace.DecidedByJev || record.Judgment == "" || record.Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("recorded ratification: %+v", record)
	}

	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.OpenActivity()
	feed := `#workstream-activity `
	p.Await("the ratification without a departure label", `[...document.querySelectorAll(`+quote(feed+"[data-kind=transition]")+`)].some(n => n.dataset.to === 'ratified-1' && n.querySelector('[data-field=decided-by]') === null)`)
	p.Await("no transition labelled as a departure", `document.querySelector(`+quote(feed+"[data-field=decided-by]")+`) === null`)
	if entries := entriesOf(t, c, intake.InboxRatification); len(entries) != 0 {
		t.Fatalf("ratification gate remains: %+v", entries)
	}
}
