package lifecycle_test

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

type journeyViewport struct {
	name          string
	width, height int64
	shortHeight   int64
	tallHeight    int64
	phone         bool
}

func (v journeyViewport) emulate(height int64) chromedp.Action {
	if v.phone {
		return chromedp.EmulateViewport(v.width, height, chromedp.EmulateMobile)
	}
	return chromedp.EmulateViewport(v.width, height)
}

type journey struct {
	*briefJourney
	other      config.WorkstreamID
	screenshot string
}

func newJourney(t *testing.T) *journey {
	t.Helper()
	j := newBriefFixture(t, listenWeb)
	first, repository := j.buildFixture().SeedGit(t, "journey")
	j.stream, j.repository = first, repository
	j.host(t)
	j.s.setSole(&activeProject{repository: repository})
	other := j.secondMember(t, "journey-other")
	j.assemble(t, first, "internal/trace", "resume", "dedupe")
	j.assemble(t, other, "internal/trace", "other-resume", "other-dedupe")
	screenshot := j.captureImage(t, "uploads", j.report.Commit, previewPicture(t))
	for n := 1; n <= 3; n++ {
		status := "passed"
		if n == 1 {
			status = "failed"
		}
		at := fixturekit.DemoStart.Add(time.Duration(n) * time.Minute)
		fixturekit.FeedDocument(t, repository, first, fmt.Sprintf("unit-resume-checks-%d", n), fmt.Sprintf("units/resume/checks-%d.json", n), 1, at,
			service.UnitCheckRun{Unit: "resume", Run: n, Status: status, StartedAt: at, FinishedAt: at})
	}
	at := fixturekit.DemoStart.Add(10 * time.Minute)
	fixturekit.FeedDocument(t, repository, first, "unit-dedupe-checks-1", "units/dedupe/checks-1.json", 1, at,
		service.UnitCheckRun{Unit: "dedupe", Run: 1, Status: "passed", StartedAt: at, FinishedAt: at})
	j.chief.plan(1, briefPlan{Text: briefText, Items: []string{briefShipFirst, briefShipNext}, Screenshots: []string{screenshot}})
	return &journey{briefJourney: j, other: other, screenshot: screenshot}
}

func TestBrowserJourneyWalksTheRedesignedPage(t *testing.T) {
	t.Parallel()
	for _, v := range []journeyViewport{
		{name: "desktop", width: 1280, height: 800, shortHeight: 420, tallHeight: 1500},
		{name: "phone", width: 375, height: 812, shortHeight: 420, tallHeight: 1900, phone: true},
	} {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			walkTheRedesignedPage(t, v)
		})
	}
}

func walkTheRedesignedPage(t *testing.T, v journeyViewport) {
	j := newJourney(t)
	stream, other := j.stream, j.other
	j.run(t)
	first, second := j.awaitEntry(t, stream), j.awaitEntry(t, other)
	card := intakefixture.DecisionCard(first)
	cardSelector := strings.TrimSpace(card)
	hash := "#workstream=" + string(stream)

	p := browsertest.OpenBrowser(t)
	audit := func(step string) { p.T.Helper(); p.Audit(v.name+": "+step, v.phone) }
	shot := func(view string, position ...string) { p.T.Helper(); p.Screenshot(v.name+"-"+view, position...) }
	p.Run(v.emulate(v.height), chromedp.Navigate("http://"+j.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)

	p.Await("the home view listing both deliveries", `(() => {
  const home = document.querySelector('[data-view=project-pane]');
  return !home.hidden && home.querySelectorAll('[data-field=needs-you] .needs-list > li').length === 2;
})()`)
	var home struct {
		Workstreams, Kinds []string
		Count              string
		Above              bool
		Cards              int
		Rows               map[string]string
	}
	p.Eval(`(() => {
  const needs = document.querySelector('[data-field=needs-you]');
  const items = [...needs.querySelectorAll('.needs-list > li')];
  const card = document.querySelector('[data-view=project-pane] .project-card');
  return {
    workstreams: items.map(i => i.dataset.needsWorkstream).sort(),
    kinds: items.map(i => i.querySelector('[data-field=kind]').textContent),
    count: needs.querySelector('[data-field=needs-count]').textContent,
    above: !!card && !!(needs.compareDocumentPosition(card) & Node.DOCUMENT_POSITION_FOLLOWING),
    cards: document.querySelectorAll('[data-view=project-pane] .project-card').length,
    rows: Object.fromEntries([...document.querySelectorAll('#workstream-list [data-select]')].map(r => [r.dataset.select, r.querySelector('[data-field=needs]')?.textContent ?? ''])),
  };
})()`, &home)
	wantStreams := []string{string(stream), string(other)}
	if wantStreams[0] > wantStreams[1] {
		wantStreams[0], wantStreams[1] = wantStreams[1], wantStreams[0]
	}
	if strings.Join(home.Workstreams, ",") != strings.Join(wantStreams, ",") || strings.Join(home.Kinds, ",") != "Mark ready for review,Mark ready for review" || home.Count != "2" || !home.Above || home.Cards != 1 {
		t.Fatalf("home: %+v, want deliveries of %v above one project card", home, wantStreams)
	}
	if home.Rows[string(stream)] != "1" || home.Rows[string(other)] != "1" {
		t.Fatalf("the sidebar counts %v, want 1 for each workstream", home.Rows)
	}
	shot("home")
	audit("home")

	p.Click(`[data-needs-workstream="` + string(stream) + `"] a`)
	p.Await("the workstream", `!document.querySelector('.view[data-view=workstream]').hidden && document.getElementById('workstream-head').dataset.workstream === `+browsertest.Quote(string(stream)))
	p.Await("both units in the nest tube", `document.querySelectorAll('#workstream-head [data-field=units] button.cell').length === 2`)
	var head struct {
		State, ID, Cells, Tab, Hash string
		Workspaces                  bool
	}
	p.Eval(`(() => {
  const h = document.getElementById('workstream-head');
  return {
    state: h.querySelector('[data-field=state]').textContent,
    id: h.querySelector('[data-field=workstream-id]').textContent,
    cells: [...h.querySelectorAll('[data-field=units] button.cell')].map(c => c.getAttribute('aria-label')).join(','),
    tab: document.querySelector('[role=tab][aria-selected=true]').dataset.tab,
    hash: location.hash,
    workspaces: h.querySelector('[data-field=goal]').textContent.includes('workspaces'),
  };
})()`, &head)
	if !strings.Contains(head.State, "assembled") || head.ID != string(stream) || head.Cells != "Unit resume: merged,Unit dedupe: merged" || head.Tab != "conversation" || head.Workspaces {
		t.Fatalf("the workstream header: %+v", head)
	}

	p.AwaitText(card+"[data-field=brief-text]", briefText)
	var conversation struct {
		Milestones, Machine, Allowed string
		Machines                     int
		CardAboveComposer            bool
		ReleaseNoteOrder             bool
		Primary                      []string
	}
	p.Eval(`(() => {
  const feed = document.querySelector('.view[data-view=workstream]');
  const card = document.querySelector(`+browsertest.Quote(cardSelector)+`);
  const composer = card.closest('article.workstream').querySelector('.composer');
  const parts = ['[data-field=brief]', '[data-field=review]', '[data-field=criteria-section]', '[data-field=preview]', 'button[type=submit]'].map(s => card.querySelector(s));
  const milestones = [...feed.querySelectorAll('[data-kind=milestone]')].map(n => n.dataset.milestone);
  return {
    milestones: milestones.join(','),
    allowed: milestones.every(m => ['ratified', 'building', 'assembled', 'final_review', 'delivered'].includes(m)) ? 'yes' : 'no',
    machines: feed.querySelectorAll('[data-panel=conversation] :is([data-kind=session], [data-kind=transition], [data-kind=unit_checks], [data-kind=final_checks], [data-kind=session_checks])').length,
    cardAboveComposer: !!(card.compareDocumentPosition(composer) & Node.DOCUMENT_POSITION_FOLLOWING),
    releaseNoteOrder: parts.every((n, i) => !!n && (i === 0 || !!(parts[i - 1].compareDocumentPosition(n) & Node.DOCUMENT_POSITION_FOLLOWING))),
    primary: [...card.querySelectorAll('button.primary')].map(b => b.textContent),
  };
})()`, &conversation)
	for _, want := range []string{"assembled", "final_review"} {
		if !strings.Contains(conversation.Milestones, want) {
			t.Fatalf("the milestones are %q, want %s among them", conversation.Milestones, want)
		}
	}
	if conversation.Allowed != "yes" || conversation.Machines != 0 || !conversation.CardAboveComposer || !conversation.ReleaseNoteOrder ||
		strings.Join(conversation.Primary, "|") != "Mark ready for review" {
		t.Fatalf("Conversation: %+v", conversation)
	}
	shot("conversation", `document.querySelector('[data-panel=conversation] [data-kind=milestone]').scrollIntoView({block: 'center'})`)
	p.Run(v.emulate(v.tallHeight))
	p.Await("the tall viewport", `innerHeight === `+fmt.Sprint(v.tallHeight))
	shot("delivery-card", `(() => {
  const card = document.querySelector(`+browsertest.Quote(cardSelector)+`);
  card.scrollIntoView({block: 'start'});
  document.getElementById('main').scrollTop -= document.querySelector('.workstream-top').getBoundingClientRect().bottom - card.getBoundingClientRect().top + 12;
})()`)
	p.Run(v.emulate(v.height))
	p.Await("the full viewport", `innerHeight === `+fmt.Sprint(v.height))
	audit("conversation")

	p.Click(`#workstream-head [data-cell=resume]`)
	activityHash := hash + "&tab=activity&unit=resume"
	p.Await("Activity on the resume unit", `!document.querySelector('[data-panel=activity]').hidden && document.querySelector('[data-tab=activity]').getAttribute('aria-selected') === 'true' && location.hash === `+browsertest.Quote(activityHash))
	p.Await("only the resume unit's events", `(() => {
  const log = document.querySelector('#workstream-activity');
  const units = [...log.querySelectorAll('[data-unit]')].map(n => n.dataset.unit);
  return log.querySelector('[data-field=unit-filter]').value === 'resume' && units.length >= 3 && units.every(u => u === 'resume');
})()`)
	var filtered struct {
		Checks, Selects int
		Folds, Open     int
		Text            string
	}
	p.Eval(`(() => {
  const log = document.querySelector('#workstream-activity');
  return {
    checks: log.querySelectorAll('[data-kind=unit_checks][data-unit=resume]').length,
    selects: document.querySelectorAll('[data-panel=activity] select').length,
    folds: log.querySelectorAll('.fold').length,
    open: log.querySelectorAll('.fold[open]').length,
    text: log.querySelector('.fold summary')?.textContent ?? '',
  };
})()`, &filtered)
	if filtered.Checks != 3 || filtered.Selects != 1 || filtered.Folds != 1 || filtered.Open != 0 || !strings.Contains(filtered.Text, "events") {
		t.Fatalf("filtered Activity: %+v, want three check runs of the unit in one closed fold and the unit filter alone", filtered)
	}
	shot("activity")
	audit("activity")

	p.Click(`#workstream-activity .fold > summary`)
	p.Await("the run of events open and named in the location", `document.querySelector('#workstream-activity .fold').open && location.hash.startsWith(`+browsertest.Quote(activityHash+"&open=")+`)`)
	var shared string
	p.Eval(`location.href`, &shared)

	p.Run(chromedp.Navigate("about:blank"), chromedp.Navigate(shared))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Await("the same Activity, unit and opened run", `(() => {
  const log = document.querySelector('#workstream-activity');
  return !document.querySelector('[data-panel=activity]').hidden && document.querySelector('[data-tab=activity]').getAttribute('aria-selected') === 'true' &&
    log.querySelector('[data-field=unit-filter]')?.value === 'resume' && log.querySelectorAll('.fold[open]').length === 1 &&
    log.querySelectorAll('[data-kind=unit_checks][data-unit=resume]').length === 3 && location.href === `+browsertest.Quote(shared)+`;
})()`)
	p.Run(chromedp.Reload())
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Await("the view again after a reload", `document.querySelector('#workstream-activity [data-field=unit-filter]')?.value === 'resume' && document.querySelectorAll('#workstream-activity .fold[open]').length === 1`)

	p.Click(`[data-tab=conversation]`)
	p.Await("Conversation and its milestones", `!document.querySelector('[data-panel=conversation]').hidden && location.hash === `+browsertest.Quote(hash)+` && document.querySelectorAll('[data-panel=conversation] [data-kind=milestone]').length >= 2`)
	p.Click(`[data-panel=conversation] [data-milestone=final_review] a`)
	p.Await("Activity at the final review", `!document.querySelector('[data-panel=activity]').hidden && location.hash.startsWith(`+browsertest.Quote(hash+"&tab=activity")+`) && location.hash.includes('&entry=') &&
    document.querySelector('#workstream-activity [data-kind=final_review]')?.dataset.focus === 'true' && document.querySelector('#workstream-activity [data-field=unit-filter]').value === ''`)
	p.Click(`[data-tab=conversation]`)
	p.Await("Conversation again", `!document.querySelector('[data-panel=conversation]').hidden && location.hash === `+browsertest.Quote(hash))

	p.Await("the delivery card ready to approve", `!document.querySelector(`+browsertest.Quote(cardSelector+" button.primary")+`).disabled`)
	drafted := j.present(t, stream)
	p.Click(card + "button.primary")
	p.AwaitOutcome("Approved marking final review 1")
	p.AwaitGone("the approved delivery", card)
	approved := j.present(t, stream)
	if approved.Approval == nil || approved.Approval.Description != drafted.Draft || !strings.Contains(approved.Approval.Description, briefBlock) {
		t.Fatalf("the recorded approval: %+v", approved.Approval)
	}
	if left := j.entries(t, other); len(left) != 1 || left[0].Number != second.Number {
		t.Fatalf("the other delivery is %+v, want it still waiting", left)
	}
	p.Await("one delivery left in the needs-you count", `document.querySelector('[data-field=needs-count]').textContent === '1' && document.querySelectorAll('.needs-list > li').length === 1`)

	p.Click("#new-workstream")
	p.Await("the New menu", `document.getElementById('create-menu').matches(':popover-open')`)
	var created []string
	p.Eval(`[...document.querySelectorAll('#create-menu button')].map(b => b.textContent.trim())`, &created)
	slices.Sort(created)
	if strings.Join(created, "|") != "Create project|Hand in a feature|Open a design session|Open an exploratory session" {
		t.Fatalf("the New menu offers %q", created)
	}
	var sidebarCreates int
	p.Eval(`document.querySelectorAll('#sidebar [data-open="project-add"], #sidebar [data-open="handin"], #sidebar [data-open="design-add"], #sidebar [data-open="session-add"]').length`, &sidebarCreates)
	if sidebarCreates != 0 {
		t.Fatalf("the sidebar still has %d create buttons", sidebarCreates)
	}
	audit("new menu")
	p.ClosePopovers()

	p.Click("#settings-button")
	p.Await("Settings", `!document.getElementById('settings-menu').hidden`)
	sections := map[string]string{"projects": "Projects", "priority": "Priority", "profiles": "Profiles and usage", "config": "Configuration", "engine": "Dagger engine"}
	for _, name := range []string{"projects", "priority", "profiles", "config", "engine"} {
		p.Click(`#settings-nav [data-open="` + name + `"]`)
		p.Await("the "+name+" section alone", `(() => {
  const shown = [...document.querySelectorAll('#settings-menu [data-section]')].filter(s => !s.hidden).map(s => s.dataset.section);
  return shown.length === 1 && shown[0] === `+browsertest.Quote(name)+` && document.querySelector('#settings-nav [data-open="`+name+`"]').getAttribute('aria-current') === 'true';
})()`)
		p.AwaitText(`#settings-menu [data-section="`+name+`"]`, sections[name])
		shot("settings-" + name)
		audit("settings " + name)
	}
	p.Await("Restart and Prune in the engine section", `!!document.querySelector('#settings-menu [data-section=engine] #engine-restart') && !!document.querySelector('#settings-menu [data-section=engine] #engine-prune')`)

	p.SelectWorkstream(other)
	openAbandon := func() {
		p.T.Helper()
		p.Click("#workstream-menu-button")
		p.Click(`[data-workstream-action="abandon"]`)
		p.Await("the abandon dialog, settled", `document.getElementById('confirm-dialog').open && document.activeElement === document.getElementById('confirm-reason') &&
    document.getElementById('confirm-dialog').getAnimations().every(a => a.playState === 'finished')`)
	}

	p.Run(v.emulate(v.shortHeight))
	p.Await("the short viewport", `innerHeight === `+fmt.Sprint(v.shortHeight))
	sticky := p.MeasureSticky()
	if sticky.Sideways || sticky.Between < 0.3*sticky.Viewport {
		t.Errorf("%s at %vpx high: the sticky header covers %v and the message field %v, leaving %v for the conversation (sideways: %v)", v.name, sticky.Viewport, sticky.Top, sticky.Bottom, sticky.Between, sticky.Sideways)
	}
	openAbandon()
	var fit struct{ Top, Bottom, Height, Accept, Cancel float64 }
	p.Eval(`(() => {
  const d = document.getElementById('confirm-dialog').getBoundingClientRect();
  const a = document.getElementById('confirm-accept'), c = document.getElementById('confirm-cancel');
  a.scrollIntoView({block: 'nearest'});
  return { top: d.top, bottom: d.bottom, height: d.height, accept: a.getBoundingClientRect().bottom, cancel: c.getBoundingClientRect().bottom };
})()`, &fit)
	if fit.Top < 0 || fit.Bottom > float64(v.shortHeight) || fit.Accept > float64(v.shortHeight) || fit.Cancel > float64(v.shortHeight) {
		t.Errorf("%s at %dpx high: the dialog does not fit the viewport: %+v", v.name, v.shortHeight, fit)
	}
	audit("abandon dialog at a short height")
	p.Click("#confirm-cancel")
	p.Await("the dialog closed", `!document.getElementById('confirm-dialog').open`)
	p.Run(v.emulate(v.height))
	p.Await("the full viewport", `innerHeight === `+fmt.Sprint(v.height))

	openAbandon()
	var dialog struct {
		Title, Consequence                 string
		Accept, Cancel                     string
		AcceptDanger, CancelQuiet, Focused bool
		Documents                          bool
	}
	p.Eval(`(() => {
  const d = document.getElementById('confirm-dialog'), a = document.getElementById('confirm-accept'), c = document.getElementById('confirm-cancel');
  return { title: d.querySelector('h2').textContent, consequence: d.textContent, accept: a.textContent, cancel: c.textContent,
    acceptDanger: a.classList.contains('danger'), cancelQuiet: c.classList.contains('quiet'), focused: document.activeElement === document.getElementById('confirm-reason'),
    documents: !document.querySelector('[data-panel=documents]').hidden };
})()`, &dialog)
	if !strings.HasPrefix(dialog.Title, "Abandon ") || !strings.Contains(dialog.Consequence, "Abandoning stops new turns") || dialog.Accept != "Abandon workstream" || dialog.Cancel != "Cancel" ||
		!dialog.AcceptDanger || !dialog.CancelQuiet || !dialog.Focused || dialog.Documents {
		t.Fatalf("the abandon dialog: %+v", dialog)
	}
	p.Screenshot(v.name + "-abandon-dialog")
	audit("abandon dialog")

	p.Click("#confirm-cancel")
	p.Await("the dialog closed and focus back on the menu button", `!document.getElementById('confirm-dialog').open && document.activeElement === document.getElementById('workstream-menu-button')`)
	statuses, err := j.c.Statuses(t.Context())
	intakefixture.Must(t, err)
	for _, w := range statuses.Workstreams {
		if w.Workstream == other && (w.State == nil || *w.State == intake.AbandonedState) {
			t.Fatalf("cancelling the dialog left the workstream %v", w.State)
		}
	}

	openAbandon()
	p.TypeInto("#confirm-reason", "Superseded by the upload design.")
	p.Await("the reason typed", `document.getElementById('confirm-reason').value === 'Superseded by the upload design.'`)
	p.Click("#confirm-accept")
	p.Await("the dialog closed", `!document.getElementById('confirm-dialog').open`)
	intakefixture.Eventually(t, "the service never abandoned the workstream the dialog confirmed", func() bool {
		statuses, err := j.c.Statuses(t.Context())
		intakefixture.Must(t, err)
		for _, w := range statuses.Workstreams {
			if w.Workstream == other {
				return w.State != nil && *w.State == intake.AbandonedState
			}
		}
		return false
	})
	p.Await("the page showing the abandonment", `document.body.innerText.includes('Abandoned')`)
	if left := j.entries(t, other); len(left) != 0 {
		t.Fatalf("an abandoned workstream still asks for delivery: %+v", left)
	}
	audit("after abandoning")
}

func previewPicture(t *testing.T) []byte {
	t.Helper()
	const width, height = 640, 400
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	fill := func(r image.Rectangle, c color.RGBA) {
		draw.Draw(picture, r, &image.Uniform{c}, image.Point{}, draw.Src)
	}
	fill(picture.Bounds(), color.RGBA{0x16, 0x1b, 0x26, 0xff})
	fill(image.Rect(0, 0, width, 44), color.RGBA{0x1e, 0x25, 0x34, 0xff})
	fill(image.Rect(24, 12, 120, 32), color.RGBA{0x5b, 0xa4, 0xe8, 0xff})
	fill(image.Rect(0, 44, 150, height), color.RGBA{0x12, 0x17, 0x21, 0xff})
	for row := 0; row < 6; row++ {
		top := 70 + row*44
		fill(image.Rect(24, top, 130, top+10), color.RGBA{0x3a, 0x44, 0x58, 0xff})
		fill(image.Rect(180, top, 600, top+30), color.RGBA{0x1e, 0x25, 0x34, 0xff})
		progress := 600 - row*70
		fill(image.Rect(180, top+22, progress, top+28), color.RGBA{0x5b, 0xa4, 0xe8, 0xff})
	}
	var data bytes.Buffer
	intakefixture.Must(t, png.Encode(&data, picture))
	return data.Bytes()
}
