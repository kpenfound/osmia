package browsertest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type journeyViewport struct {
	name          string
	width, height int64
	phone         bool
}

var journeyViewports = []journeyViewport{
	{name: "desktop", width: 1280, height: 800},
	{name: "phone", width: 390, height: 844, phone: true},
}

func (v journeyViewport) emulate() chromedp.Action {
	if v.phone {
		return chromedp.EmulateViewport(v.width, v.height, chromedp.EmulateMobile)
	}
	return chromedp.EmulateViewport(v.width, v.height)
}

func TestBrowserJourneyRatifiesFromTheNeedsYouListAndShowsTheNestTube(t *testing.T) {
	t.Parallel()
	for _, v := range journeyViewports {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			ratifyFromHome(t, v)
		})
	}
}

func ratifyFromHome(t *testing.T, v journeyViewport) {
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
	ws := scenario.ShedAs(t, "journey")
	entries := entriesOf(t, c, intake.InboxRatification)
	if len(entries) != 1 || entries[0].Workstream != ws {
		t.Fatalf("ratification entries: %+v", entries)
	}
	card := decisionCard(entries[0])
	cardSelector := strings.TrimSpace(card)
	hash := "#workstream=" + string(ws)
	audit := func(step string) { p.T.Helper(); p.Audit(v.name+": "+step, v.phone) }
	shot := func(view string, position ...string) { p.T.Helper(); p.Screenshot(v.name+"-"+view, position...) }

	p.Run(v.emulate(), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)

	p.Await("the ratification on home", `(() => {
  const items = document.querySelectorAll('[data-view=project-pane] [data-field=needs-you] .needs-list > li');
  return items.length === 1 && items[0].dataset.needsWorkstream === `+quote(string(ws))+` && document.querySelector('[data-field=needs-count]').textContent === '1';
})()`)
	audit("home")
	p.Click(`[data-needs-workstream="` + string(ws) + `"] a`)
	p.Await("the workstream", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(ws))+` && document.querySelector(`+quote(cardSelector)+`) !== null`)

	p.AwaitText(card+`[data-decision-choice="ratify"]`, "Ratify")
	var shown struct {
		QuestionFirst, Closed bool
		Primary, Secondary    []string
	}
	p.Eval(`(() => {
  const card = document.querySelector(`+quote(cardSelector)+`);
  const buttons = [...card.querySelectorAll('[data-decision-choice]')];
  const question = card.querySelector('[data-field=question]');
  const follows = (a, b) => !!(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
  return {
    questionFirst: buttons.every(b => follows(question, b)) && follows(question, card.querySelector('.card-meta')),
    closed: [...card.querySelectorAll('details.footnote')].every(d => !d.open),
    primary: buttons.filter(b => b.classList.contains('primary')).map(b => b.dataset.decisionChoice),
    secondary: buttons.filter(b => b.classList.contains('secondary')).map(b => b.dataset.decisionChoice),
  };
})()`, &shown)
	if !shown.QuestionFirst || !shown.Closed || strings.Join(shown.Primary, ",") != "ratify" || strings.Join(shown.Secondary, ",") != "redraft" {
		t.Fatalf("the ratification card: %+v", shown)
	}
	shot("decision-card", `(() => {
  const card = document.querySelector(`+quote(cardSelector)+`);
  card.scrollIntoView({block: 'start'});
  document.getElementById('main').scrollTop -= document.querySelector('.workstream-top').getBoundingClientRect().bottom - card.getBoundingClientRect().top + 12;
})()`)
	audit("decision card")

	p.Click(card + `[data-decision-choice="ratify"]`)
	p.AwaitOutcome("the owner ratified spec.md revision 1 and plan.json revision 1")
	p.AwaitGone("the ratified packet", card)
	scenario.AwaitFeature(t, ws, intake.BuildingState)
	p.Await("a milestone for the ratified workstream", `[...document.querySelectorAll('[data-panel=conversation] [data-kind=milestone]')].some(n => ['ratified', 'building'].includes(n.dataset.milestone))`)
	audit("conversation after ratifying")

	p.Await("a cell for each unit", `document.querySelectorAll('#workstream-head [data-field=units] button.cell').length === 2`)
	var cells []string
	p.Eval(`[...document.querySelectorAll('#workstream-head [data-field=units] button.cell')].map(c => c.dataset.cell + '|' + c.getAttribute('aria-label').startsWith('Unit ' + c.dataset.cell + ': '))`, &cells)
	if strings.Join(cells, ",") != "resume|true,dedupe|true" {
		t.Fatalf("the nest tube cells: %q", cells)
	}
	p.Click(`#workstream-head [data-cell=dedupe]`)
	activity := hash + "&tab=activity&unit=dedupe"
	p.Await("Activity on the dedupe unit", `!document.querySelector('[data-panel=activity]').hidden && location.hash === `+quote(activity)+` && document.querySelector('#workstream-activity [data-field=unit-filter]')?.value === 'dedupe'`)
	var units []string
	p.Eval(`[...document.querySelectorAll('#workstream-activity [data-field=unit-filter] option')].map(o => o.textContent)`, &units)
	if strings.Join(units, ",") != "All units,dedupe,resume" {
		t.Fatalf("the unit filter offers %q", units)
	}
	audit("activity")
	p.Run(chromedp.Reload())
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Await("the filter restored after a reload", `!document.querySelector('[data-panel=activity]').hidden && document.querySelector('#workstream-activity [data-field=unit-filter]')?.value === 'dedupe' && location.hash === `+quote(activity))
	p.Click(`[data-tab=conversation]`)
	p.Await("Conversation again", `!document.querySelector('[data-panel=conversation]').hidden && location.hash === `+quote(hash))
}

func TestBrowserJourneyOpensADesignSessionAndReadsItsBrief(t *testing.T) {
	t.Parallel()
	for _, v := range journeyViewports {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			designFromTheNewMenu(t, v)
		})
	}
}

func designFromTheNewMenu(t *testing.T, v journeyViewport) {
	p := openBrowser(t)
	s, _, home, engine := newDesignEnforcementFixture(t, true)
	const brief = "Decisions: uploads resume from the last chunk the server acknowledged.\nIntent and tradeoffs: favour correctness over speed.\nRejected options: restarting every upload."
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		out, err := callTool(ctx, tools, draftBriefToolName, map[string]any{"content": brief, "projects": []config.ProjectID{home}})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("draft: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "design-session", ResultText: "I drafted a brief: uploads resume from the last acknowledged chunk."}, nil
	}
	engine.Mu.Unlock()
	audit := func(step string) { p.T.Helper(); p.Audit(v.name+": "+step, v.phone) }
	p.Run(v.emulate(), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)

	p.OpenNewItem("#sidebar-new-design")
	p.Await("the design form", `!document.querySelector('[data-view=design-add]').hidden`)
	p.Choose("#design-add-form [name=project]", string(home))
	audit("design form")
	p.Click("#design-add-form [type=submit]")
	p.AwaitText("#design-add-form .result", "Opened design session")
	p.Await("the design view", `!document.querySelector('[data-view=design]').hidden`)
	p.TypeInto("#design-form [name=text]", "Draft the brief for resumable uploads.")
	p.Click("#design-form [type=submit]")
	p.AwaitText("#design-brief", "Brief revision: 1")
	p.AwaitText("#design-feed", "I drafted a brief")

	controls := []string{"#design-brief", "#design-start", "#design-destroy"}
	wide, stacked := splitLayout(p, "#design-feed", controls...)
	if v.phone && (wide || !stacked) || !v.phone && (!wide || stacked) {
		t.Fatalf("%s: two columns %v, stacked %v", v.name, wide, stacked)
	}
	p.Screenshot(v.name+"-design-view", `document.getElementById('main').scrollTop = 0`)
	audit("design view")
}
