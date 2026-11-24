package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const nestUnits = `[{"unit":"resume","state":"merged"},{"unit":"upload","state":"implementing"},` +
	`{"unit":"audit","state":"ready"},{"unit":"index","state":"planned"},` +
	`{"unit":"dedupe","state":"contested","reason":"checks failed three times"}]`

func openTubePage(t *testing.T, units string) (*page, string) {
	t.Helper()
	api := newFakeAPI(1)
	api.units = units
	api.setFeed("w_000",
		chatEntry("message", 1, "Start the upload work."),
		unitTransitionEntry(2, "upload", "ready", "implementing", "a mason started"),
		sessionEntry(3, "upload", "done", "Built the upload unit."),
		unitTransitionEntry(4, "dedupe", "implementing", "contested", "checks failed three times"),
		sessionEntry(6, "dedupe", "failed", "the sandbox could not start"),
	)
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.click(`[data-select="w_000"]`)
	p.await("the workstream shown", `document.getElementById('workstream-head').dataset.workstream === 'w_000'`)
	return p, server.URL
}

func openFiveUnitTube(t *testing.T) *page {
	t.Helper()
	p, _ := openTubePage(t, nestUnits)
	p.await("five cells", `document.querySelectorAll('#workstream-head [data-field=units] [data-unit-state]').length === 5`)
	return p
}

type cellLook struct {
	Colors, Widths map[string]string
	Mark           string
	Marked         int
}

func readCellLook(p *page) cellLook {
	var look cellLook
	p.eval(`(() => {
  const look = { colors: {}, widths: {}, mark: '', marked: 0 };
  for (const cell of document.querySelectorAll('#workstream-head [data-field=units] .cell')) {
    const body = getComputedStyle(cell.querySelector('.cell-body'));
    look.colors[cell.dataset.unitState] = body.backgroundColor;
    look.widths[cell.dataset.unitState] = body.borderTopWidth;
    const content = getComputedStyle(cell.querySelector('.cell-body'), '::after').content;
    if (content !== 'none' && content !== 'normal') {
      look.marked++;
      look.mark = cell.dataset.unitState + ':' + content;
    }
  }
  return look;
 })()`, &look)
	return look
}

func TestBrowserNestTubeShowsOneCellPerUnit(t *testing.T) {
	t.Parallel()
	p := openFiveUnitTube(t)
	var cells []string
	p.eval(`[...document.querySelectorAll('#workstream-head [data-field=units] button.cell')].map(c => c.dataset.unitState + '|' + c.getAttribute('aria-label') + '|' + c.dataset.cell)`, &cells)
	want := []string{
		"merged|Unit resume: merged|resume",
		"implementing|Unit upload: implementing|upload",
		"ready|Unit audit: ready|audit",
		"planned|Unit index: planned|index",
		"contested|Unit dedupe: contested|dedupe",
	}
	if strings.Join(cells, ",") != strings.Join(want, ",") {
		t.Fatalf("the cells are %q, want %q", cells, want)
	}
}

func TestBrowserNestTubeHeaderDoesNotCountUnitsByState(t *testing.T) {
	t.Parallel()
	p := openFiveUnitTube(t)
	var head struct {
		Tags, Text string
		Counted    bool
	}
	p.eval(`({
  tags: [...document.querySelectorAll('#workstream-head .tag')].map(t => t.textContent).join(','),
  text: document.getElementById('workstream-head').textContent,
  counted: /\b(planned|implementing|ready|merged|contested) \d/.test(document.getElementById('workstream-head').textContent),
 })`, &head)
	if head.Counted || head.Tags != "" {
		t.Fatalf("the header counts units by state: tags %q, text %q", head.Tags, head.Text)
	}
}

func TestBrowserNestTubeCellsOfDifferentStatesHaveDifferentColours(t *testing.T) {
	t.Parallel()
	look := readCellLook(openFiveUnitTube(t))
	if len(look.Colors) != 5 {
		t.Fatalf("cell colours %v", look.Colors)
	}
	seen := map[string]string{}
	for state, color := range look.Colors {
		if other, dup := seen[color]; dup {
			t.Fatalf("the %s and %s cells share the colour %s", state, other, color)
		}
		seen[color] = state
	}
}

func TestBrowserNestTubeMarksOnlyTheContestedCell(t *testing.T) {
	t.Parallel()
	look := readCellLook(openFiveUnitTube(t))
	if look.Marked != 1 || look.Mark != `contested:"!"` {
		t.Fatalf("marked cells %d, mark %q; want only the contested cell marked with !", look.Marked, look.Mark)
	}
}

func TestBrowserNestTubeGivesOnlyTheContestedCellADifferentEdge(t *testing.T) {
	t.Parallel()
	look := readCellLook(openFiveUnitTube(t))
	for state, width := range look.Widths {
		if (state == "contested") != (width != "1px") {
			t.Fatalf("the %s cell has a %s edge", state, width)
		}
	}
}

func TestBrowserNestTubeCellOpensActivityOnItsUnit(t *testing.T) {
	t.Parallel()
	p, _ := openTubePage(t, nestUnits)
	cell := func(unit string) string { return `#workstream-head [data-cell=` + unit + `]` }
	readout := `document.querySelector('#workstream-head [data-field=unit-name]').textContent`

	p.await("the count before any cell is named", readout+` === '5 units'`)
	var x, y float64
	p.eval(`(() => { const r = document.querySelector(`+quote(cell("upload"))+`).getBoundingClientRect(); return r.left + r.width / 2; })()`, &x)
	p.eval(`(() => { const r = document.querySelector(`+quote(cell("upload"))+`).getBoundingClientRect(); return r.top + r.height / 2; })()`, &y)
	p.run(chromedp.ActionFunc(func(ctx context.Context) error { return input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx) }))
	p.await("the hovered unit named", readout+` === 'uploadimplementing'`)
	p.run(chromedp.ActionFunc(func(ctx context.Context) error { return input.DispatchMouseEvent(input.MouseMoved, 5, 400).Do(ctx) }))
	p.await("the name cleared", readout+` === '5 units'`)
	p.run(chromedp.Focus(cell("dedupe"), chromedp.ByQuery))
	p.await("the focused unit named", readout+` === 'dedupecontested'`)

	p.click(cell("upload"))
	p.await("Activity on the upload unit", `!document.querySelector('[data-panel=activity]').hidden && document.querySelector('[data-tab=activity]').getAttribute('aria-selected') === 'true' && location.hash === '#workstream=w_000&tab=activity&unit=upload'`)
	p.await("only the upload unit's events", `document.querySelector('#workstream-activity [data-field=unit-filter]').value === 'upload' &&
		document.querySelectorAll('#workstream-activity [data-unit=upload]').length > 0 && !document.querySelector('#workstream-activity [data-unit=dedupe]')`)

	p.run(chromedp.Reload())
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.await("the unit filter restored", `!document.querySelector('[data-panel=activity]').hidden && document.querySelector('#workstream-activity [data-field=unit-filter]')?.value === 'upload'`)

	p.openTab("conversation")
	p.run(chromedp.Focus(cell("dedupe"), chromedp.ByQuery), chromedp.KeyEvent("\r"))
	p.await("Activity on the dedupe unit", `location.hash === '#workstream=w_000&tab=activity&unit=dedupe'`)
}

type headerFacts struct {
	Pill, Glyph, ID, IDFont, Headline, Workspaces string
	WorkspacesInTitle                             bool
}

func readHeader(p *page) headerFacts {
	var head headerFacts
	p.eval(`(() => {
  const h = document.getElementById('workstream-head');
  const id = h.querySelector('[data-field=workstream-id]');
  const pill = h.querySelector('[data-field=state]');
  return {
    pill: pill.textContent,
    glyph: pill.querySelector('.glyph').dataset.glyph,
    id: id.textContent,
    idFont: getComputedStyle(id).fontFamily,
    headline: h.querySelector('[data-field=goal]').textContent,
    workspaces: h.querySelector('[data-field=workspaces]').textContent,
    workspacesInTitle: h.querySelector('[data-field=goal]').textContent.includes('workspaces') || h.querySelector('.head-meta').textContent.includes('workspaces'),
  };
 })()`, &head)
	return head
}

func TestBrowserHeaderShowsTheStatePill(t *testing.T) {
	t.Parallel()
	p, _ := openTubePage(t, nestUnits)
	head := readHeader(p)
	if head.Pill != "building" || head.Glyph != "live" {
		t.Fatalf("the state pill shows %q with the glyph %q", head.Pill, head.Glyph)
	}
}

func TestBrowserHeaderShowsTheWorkstreamIDInMono(t *testing.T) {
	t.Parallel()
	p, _ := openTubePage(t, nestUnits)
	head := readHeader(p)
	if head.ID != "w_000" || !strings.Contains(head.IDFont, "JetBrains Mono") {
		t.Fatalf("the ID shows %q in %q", head.ID, head.IDFont)
	}
}

func TestBrowserHeaderKeepsTheWorkspaceCountOutOfTheHeadline(t *testing.T) {
	t.Parallel()
	p, _ := openTubePage(t, nestUnits)
	head := readHeader(p)
	if head.WorkspacesInTitle || head.Workspaces != "git workspaces" {
		t.Fatalf("the workspace count %q is in the headline: %v", head.Workspaces, head.WorkspacesInTitle)
	}
}

func TestBrowserHeaderCopiesTheWorkstreamID(t *testing.T) {
	t.Parallel()
	p, url := openTubePage(t, nestUnits)
	p.run(chromedp.ActionFunc(func(ctx context.Context) error {
		executor := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
		for _, name := range []string{"clipboard-read", "clipboard-write"} {
			grant := browser.SetPermission(&browser.PermissionDescriptor{Name: name}, browser.PermissionSettingGranted).WithOrigin(url)
			if err := grant.Do(executor); err != nil {
				return err
			}
		}
		return nil
	}))
	p.click(`#workstream-head [data-copy-id]`)
	p.await("the copy confirmed", `document.querySelector('#workstream-head [data-copy-id]').dataset.copied === 'true'`)
	var copied string
	p.run(chromedp.Evaluate(`navigator.clipboard.readText()`, &copied, func(params *runtime.EvaluateParams) *runtime.EvaluateParams { return params.WithAwaitPromise(true) }))
	if copied != "w_000" {
		t.Fatalf("the clipboard holds %q, want the workstream ID", copied)
	}
}

func TestBrowserNestTubeCellsStayTappableOnAPhone(t *testing.T) {
	t.Parallel()
	var units []string
	for n := 0; n < 14; n++ {
		units = append(units, fmt.Sprintf(`{"unit":"unit-%02d","state":"planned"}`, n))
	}
	p, _ := openTubePage(t, "["+strings.Join(units, ",")+"]")
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile))
	var tube struct {
		Cells, Rows           int
		MinWidth, MinHeight   float64
		Overflow, CellsInView bool
	}
	p.await("fourteen cells", `document.querySelectorAll('#workstream-head button.cell').length === 14`)
	p.eval(`(() => {
  const cells = [...document.querySelectorAll('#workstream-head button.cell')].map(c => c.getBoundingClientRect());
  const width = document.documentElement.clientWidth;
  return {
    cells: cells.length,
    rows: new Set(cells.map(r => Math.round(r.top))).size,
    minWidth: Math.min(...cells.map(r => r.width)),
    minHeight: Math.min(...cells.map(r => r.height)),
    overflow: document.documentElement.scrollWidth > width,
    cellsInView: cells.every(r => r.left >= 0 && r.right <= width),
  };
 })()`, &tube)
	if tube.MinWidth < 44 || tube.MinHeight < 44 {
		t.Fatalf("the smallest cell is %vx%v, want at least 44x44", tube.MinWidth, tube.MinHeight)
	}
	if tube.Rows < 2 || tube.Overflow || !tube.CellsInView {
		t.Fatalf("the tube at phone width: %+v, want it wrapped and inside the page", tube)
	}
}
