package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

func climbFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *fakeAPI) handleClimb(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, read := range []string{"progress", "attempts"} {
		if strings.HasSuffix(r.URL.Path, "/"+read) && f.climb[read] != "" {
			writeJSON(w, f.climb[read])
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	writeJSON(w, `{"error":{"code":"conflict","message":"the climb cannot be read yet"}}`)
}

func (f *fakeAPI) setClimb(progress, attempts, summary, spark string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.climb = map[string]string{"progress": progress, "attempts": attempts}
	f.climbExtra = `,"work_kind":"hill-climb","progress":{"panel":"climb","summary":` + quote(summary) + `,"spark":` + spark + `}`
}

type climbPage struct {
	*page
	api *fakeAPI
}

func openClimbPage(t *testing.T, direction string) *climbPage {
	t.Helper()
	api := newFakeAPI(1)
	progress := climbFixture(t, "climb_progress_"+direction)
	api.setClimb(progress, climbFixture(t, "climb_attempts_"+direction), summaryOf(t, progress), sparkOf(t, progress))
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 900),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := cdppage.AddScriptToEvaluateOnNewDocument(`window.__violations = [];
document.addEventListener('securitypolicyviolation', (e) => window.__violations.push(e.violatedDirective + ' ' + e.blockedURI));`).Do(ctx)
			return err
		}),
		chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.click(`[data-select="w_000"]`)
	p.await("the workstream shown", `document.getElementById('workstream-head').dataset.workstream === 'w_000'`)
	p.await("the climb panel", `document.querySelector('#climb-panel [data-field=climb-chart]') !== null`)
	return &climbPage{page: p, api: api}
}

func summaryOf(t *testing.T, progress string) string {
	t.Helper()
	return between(t, progress, `"summary": "`, `"`)
}

func sparkOf(t *testing.T, progress string) string {
	t.Helper()
	spark := between(t, progress, `"spark": [`, `]`)
	return "[" + strings.Join(strings.Fields(spark), "") + "]"
}

func between(t *testing.T, s, from, to string) string {
	t.Helper()
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("the fixture lacks %q", from)
	}
	rest := s[i+len(from):]
	j := strings.Index(rest, to)
	if j < 0 {
		t.Fatalf("the fixture lacks %q after %q", to, from)
	}
	return rest[:j]
}

func TestBrowserClimbPanelReplacesTheUnitsBarWithTheHeaderAndMeters(t *testing.T) {
	t.Parallel()
	p := openClimbPage(t, "lower")
	var shown struct {
		Units          bool
		Panel          bool
		Goal           string
		Metric         string
		Direction      string
		Progress       string
		Target         string
		Noise          string
		Attempts       string
		Cost           string
		Time           string
		Stale          string
		Summary        string
		AttemptsBar    float64
		AttemptsBarMax float64
		TimeBar        float64
	}
	p.eval(`(() => {
  const q = (s) => document.querySelector(s);
  const t = (s) => (q(s)?.textContent ?? '').replace(/\s+/g, ' ').trim();
  return {
    units: q('#workstream-head [data-field=units]') !== null,
    panel: q('#climb-panel') !== null && !q('#climb-panel').hidden,
    goal: t('#climb-panel [data-field=goal]'),
    metric: t('#climb-panel [data-field=metric]'),
    direction: t('#climb-panel [data-field=direction]'),
    progress: t('#climb-panel [data-field=progress]'),
    target: t('#climb-panel [data-field=target]'),
    noise: t('#climb-panel [data-field=noise]'),
    attempts: t('#climb-panel [data-meter=attempts] [data-field=value]'),
    cost: t('#climb-panel [data-meter=cost] [data-field=value]'),
    time: t('#climb-panel [data-meter=time] [data-field=value]'),
    stale: t('#climb-panel [data-meter=stale] [data-field=value]'),
    summary: t('#workstream-head [data-field=climb-summary]'),
    attemptsBar: q('#climb-panel [data-meter=attempts] progress')?.value ?? -1,
    attemptsBarMax: q('#climb-panel [data-meter=attempts] progress')?.max ?? -1,
    timeBar: q('#climb-panel [data-meter=time] progress')?.value ?? -1,
  };
 })()`, &shown)
	if shown.Units || !shown.Panel {
		t.Fatalf("units bar present %v, climb panel shown %v", shown.Units, shown.Panel)
	}
	if shown.Goal != "Make the parser at least 30% faster on the corpus." || !strings.Contains(shown.Metric, "wall_ms") || shown.Direction != "lower is better" {
		t.Fatalf("goal %q, metric %q, direction %q", shown.Goal, shown.Metric, shown.Direction)
	}
	if !strings.Contains(shown.Progress, "100 ms → 85 ms") || !strings.Contains(shown.Progress, "−15 ms") || !strings.Contains(shown.Progress, "−15.0%") {
		t.Fatalf("baseline to best %q", shown.Progress)
	}
	if !strings.HasSuffix(shown.Target, "70 ms") || !strings.HasSuffix(shown.Noise, "2 ms") {
		t.Fatalf("target %q, noise band %q", shown.Target, shown.Noise)
	}
	if shown.Attempts != "6 of 20" || shown.Cost != "$4.25 of $15.00" || shown.Time != "31m of 2h 0m" || shown.Stale != "2 of 5" {
		t.Fatalf("meters: attempts %q, cost %q, time %q, since improvement %q", shown.Attempts, shown.Cost, shown.Time, shown.Stale)
	}
	if shown.AttemptsBar != 6 || shown.AttemptsBarMax != 20 || shown.TimeBar != 1860 {
		t.Fatalf("the bars hold %v of %v and %v", shown.AttemptsBar, shown.AttemptsBarMax, shown.TimeBar)
	}
	if shown.Summary != "6 attempts · attempt 7 measuring 2/3 · best −15.0%" {
		t.Fatalf("header summary %q", shown.Summary)
	}
}

func TestBrowserClimbPanelShowsTheAttemptInFlight(t *testing.T) {
	t.Parallel()
	p := openClimbPage(t, "lower")
	var now struct {
		Attempt    string
		Phase      string
		Hypothesis string
		Elapsed    string
		PhaseAttr  string
	}
	p.eval(`(() => {
  const r = document.querySelector('#climb-panel [data-field=climb-running]');
  return {
    attempt: r.querySelector('[data-field=attempt]').textContent,
    phase: r.querySelector('[data-field=phase]').textContent,
    phaseAttr: r.dataset.phase,
    hypothesis: r.querySelector('[data-field=hypothesis]').textContent,
    elapsed: r.querySelector('[data-field=elapsed]').textContent,
  };
 })()`, &now)
	if now.Attempt != "Attempt 7" || now.Phase != "measuring 2/3" || now.PhaseAttr != "measuring" || now.Hypothesis != "Memoise the line index" || now.Elapsed == "" {
		t.Fatalf("now running: %+v", now)
	}
}

func TestBrowserClimbAttemptTableListsEveryAttemptWithItsLinks(t *testing.T) {
	t.Parallel()
	p := openClimbPage(t, "lower")
	var rows []string
	p.eval(`[...document.querySelectorAll('#climb-panel tr[data-attempt]')].map((r) => [r.dataset.attempt, r.dataset.class,
  r.querySelector('[data-field=outcome]').textContent, r.querySelector('[data-field=score]').textContent,
  [...r.querySelectorAll('[data-link]')].map((l) => l.dataset.link + ':' + l.textContent).join('+')].join('|'))`, &rows)
	want := []string{
		"1|kept|kept|92|diff:Diff+branch:Branch+output:Output",
		"2|discarded|discarded, no improvement|95|diff:Diff+branch:Branch+output:Output",
		"3|failed|failed, no candidate|none|diff:Diff+branch:Branch+output:Output",
		"4|kept|kept|85|diff:Diff+branch:Branch+output:Output",
		"5|discarded|discarded, review rejected|70|diff:Diff+branch:Branch+output:Output",
		"6|failed|failed, invalid measurement|none|diff:Diff+branch:Branch+output:Output",
	}
	if strings.Join(rows, "\n") != strings.Join(want, "\n") {
		t.Fatalf("attempt rows:\n%s\nwant:\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
}

func TestBrowserClimbAttemptLinksOpenTheDiffBranchAndOutput(t *testing.T) {
	t.Parallel()
	p := openClimbPage(t, "lower")
	row := `#climb-panel tr[data-attempt="5"]`
	p.click(row + ` [data-link=output]`)
	p.await("the measurement output", `document.querySelector('#climb-panel [data-attempt-detail="5"] [data-section=output] pre')?.textContent.includes('"wall_ms"')`)
	p.awaitText(`#climb-panel [data-attempt-detail="5"] [data-section=output]`, "The change hard-codes the corpus.")
	p.click(row + ` [data-link=diff]`)
	p.await("the diff files", `document.querySelector('#climb-panel [data-attempt-detail="5"] [data-section=diff] code')?.textContent === 'internal/parse/lexer.go'`)
	p.awaitText(`#climb-panel [data-attempt-detail="5"] [data-field=diff-command]`, "git diff c04 cand05")
	p.click(row + ` [data-link=branch]`)
	p.await("the branch", `document.querySelector('#climb-panel [data-attempt-detail="5"] [data-field=branch]')?.textContent === 'osmia/climb/attempt-5'`)
	var details int
	p.eval(`document.querySelectorAll('#climb-panel [data-attempt-detail]').length`, &details)
	if details != 1 {
		t.Fatalf("%d attempts are open, want only attempt 5", details)
	}
}

type chartLook struct {
	Dots       map[string]int
	Fills      map[string]string
	Strokes    map[string]string
	Better     string
	BestStart  float64
	BestEnd    float64
	BaselineY  float64
	TargetY    float64
	BandTop    float64
	BandBottom float64
	KeptY      map[string]float64
	Path       string
	XAxis      string
	Scripts    int
}

func readChart(p *page) chartLook {
	var look chartLook
	p.eval(`(() => {
  const chart = document.querySelector('#climb-panel [data-field=climb-chart]');
  const look = { dots: {}, fills: {}, strokes: {}, keptY: {} };
  for (const dot of chart.querySelectorAll('g.dot')) {
    const cls = dot.dataset.class;
    look.dots[cls] = (look.dots[cls] || 0) + 1;
    const body = getComputedStyle(dot.querySelector('.dot-body'));
    look.fills[cls] = body.fill;
    look.strokes[cls] = body.stroke;
    if (cls === 'kept') look.keptY[dot.dataset.attempt] = dot.transform.baseVal.getItem(0).matrix.f;
  }
  look.better = chart.dataset.better + ':' + chart.querySelector('[data-field=better]').textContent;
  const line = (f) => chart.querySelector('[data-field=' + f + ']');
  look.baselineY = parseFloat(line('baseline-line').getAttribute('y1'));
  look.targetY = parseFloat(line('target-line').getAttribute('y1'));
  const band = line('noise-band');
  look.bandTop = parseFloat(band.getAttribute('y'));
  look.bandBottom = look.bandTop + parseFloat(band.getAttribute('height'));
  look.path = line('best-line').getAttribute('d');
  const vs = [...look.path.matchAll(/V([\d.]+)/g)].map((m) => parseFloat(m[1]));
  look.bestStart = parseFloat(look.path.match(/^M\S+ ([\d.]+)/)[1]);
  look.bestEnd = vs[vs.length - 1];
  look.xAxis = chart.dataset.x;
  look.scripts = document.querySelectorAll('script[src]:not([src^="/"])').length;
  return look;
 })()`, &look)
	return look
}

func assertBetterIsUp(t *testing.T, look chartLook, direction string) {
	t.Helper()
	if look.Better != "up:Better ↑" {
		t.Fatalf("the chart says %q", look.Better)
	}
	if !(look.TargetY < look.BaselineY) {
		t.Fatalf("%s: the target is at %v and the baseline at %v; a target beyond the baseline must be higher on the chart", direction, look.TargetY, look.BaselineY)
	}
	if !(look.BestEnd < look.BestStart) {
		t.Fatalf("%s: the best-so-far line runs from %v to %v; improvement must rise", direction, look.BestStart, look.BestEnd)
	}
	for attempt, y := range look.KeptY {
		if !(y < look.BaselineY) {
			t.Fatalf("%s: kept attempt %s is at %v, not above the baseline at %v", direction, attempt, y, look.BaselineY)
		}
	}
	if !(look.BandTop < look.BaselineY && look.BandBottom > look.BaselineY) {
		t.Fatalf("%s: the noise band spans %v to %v, not around the baseline at %v", direction, look.BandTop, look.BandBottom, look.BaselineY)
	}
	if !strings.HasPrefix(look.Path, "M") || !strings.Contains(look.Path, " H") || !strings.Contains(look.Path, " V") {
		t.Fatalf("the best-so-far line is not a step line: %q", look.Path)
	}
}

func assertOutcomeDots(t *testing.T, look chartLook, kept, discarded, failed int) {
	t.Helper()
	if look.Dots["kept"] != kept || look.Dots["discarded"] != discarded || look.Dots["failed"] != failed {
		t.Fatalf("dots by outcome %v, want %d kept, %d discarded, %d failed", look.Dots, kept, discarded, failed)
	}
	seen := map[string]string{}
	for cls := range look.Dots {
		paint := look.Fills[cls] + "/" + look.Strokes[cls]
		if other, dup := seen[paint]; dup {
			t.Fatalf("the %s and %s dots look the same: %s", cls, other, paint)
		}
		seen[paint] = cls
	}
}

func TestBrowserClimbChartDrawsOutcomeDotsStepLineAndReferencesForAMetricThatFalls(t *testing.T) {
	t.Parallel()
	p := openClimbPage(t, "lower")
	look := readChart(p.page)
	assertOutcomeDots(t, look, 2, 2, 2)
	assertBetterIsUp(t, look, "lower")
	if look.Scripts != 0 {
		t.Fatalf("%d scripts load from outside the page's origin", look.Scripts)
	}
}

func TestBrowserClimbChartDrawsTheSameForAMetricThatRises(t *testing.T) {
	t.Parallel()
	p := openClimbPage(t, "higher")
	look := readChart(p.page)
	assertOutcomeDots(t, look, 2, 1, 0)
	assertBetterIsUp(t, look, "higher")
	var phase string
	p.eval(`document.querySelector('#climb-panel [data-field=climb-running] [data-field=phase]').textContent`, &phase)
	if phase != "mason working" {
		t.Fatalf("phase %q", phase)
	}
}

func TestBrowserClimbChartTogglesToWallClockTime(t *testing.T) {
	t.Parallel()
	p := openClimbPage(t, "lower")
	xs := func() []float64 {
		var out []float64
		p.eval(`[...document.querySelectorAll('#climb-panel g.dot')].map((d) => d.transform.baseVal.getItem(0).matrix.e)`, &out)
		return out
	}
	byNumber := xs()
	var pressed string
	p.eval(`document.querySelector('#climb-panel [data-x=attempt]').getAttribute('aria-pressed') + '/' + document.querySelector('#climb-panel [data-x=time]').getAttribute('aria-pressed')`, &pressed)
	if pressed != "true/false" {
		t.Fatalf("toggle starts at %q", pressed)
	}
	p.click(`#climb-panel [data-x=time]`)
	p.await("the wall-clock axis", `document.querySelector('#climb-panel [data-field=climb-chart]').dataset.x === 'time' && document.querySelector('#climb-panel [data-field=x-label]').textContent === 'Wall-clock time'`)
	p.eval(`document.querySelector('#climb-panel [data-x=attempt]').getAttribute('aria-pressed') + '/' + document.querySelector('#climb-panel [data-x=time]').getAttribute('aria-pressed')`, &pressed)
	if pressed != "false/true" {
		t.Fatalf("toggle after the press %q", pressed)
	}
	byTime := xs()
	if len(byTime) != len(byNumber) {
		t.Fatalf("%d dots by time, %d by number", len(byTime), len(byNumber))
	}
	same := true
	for i := range byTime {
		if byTime[i] != byNumber[i] {
			same = false
		}
	}
	if same {
		t.Fatalf("the dots did not move: %v", byTime)
	}
	var focused string
	p.eval(`document.activeElement?.dataset.x ?? ''`, &focused)
	if focused != "time" {
		t.Fatalf("focus is on %q after the press", focused)
	}
	p.click(`#climb-panel [data-x=attempt]`)
	p.await("the attempt axis", `document.querySelector('#climb-panel [data-field=x-label]').textContent === 'Attempt'`)
}

func TestBrowserClimbChartDotOpensItsAttempt(t *testing.T) {
	t.Parallel()
	p := openClimbPage(t, "lower")
	p.click(`#climb-panel g.dot[data-attempt="4"] circle.dot-hit`)
	p.await("attempt 4 opened", `document.querySelector('#climb-panel tr[data-attempt="4"]')?.getAttribute('aria-selected') === 'true' &&
		document.querySelector('#climb-panel [data-attempt-detail="4"]') !== null && document.querySelector('#climb-panel g.dot[data-attempt="4"]').getAttribute('aria-pressed') === 'true'`)
	var inView bool
	p.eval(`(() => { const r = document.querySelector('#climb-panel tr[data-attempt="4"]').getBoundingClientRect(); return r.top >= 0 && r.bottom <= innerHeight + 1; })()`, &inView)
	if !inView {
		t.Fatal("the opened attempt was not brought into view")
	}
	p.run(chromedp.Focus(`#climb-panel g.dot[data-attempt="2"]`, chromedp.ByQuery), chromedp.KeyEvent("\r"))
	p.await("attempt 2 opened by keyboard", `document.querySelector('#climb-panel tr[data-attempt="2"]')?.getAttribute('aria-selected') === 'true' && document.querySelector('#climb-panel [data-attempt-detail="4"]') === null`)
	var label string
	p.eval(`document.querySelector('#climb-panel g.dot[data-attempt="2"]').getAttribute('aria-label')`, &label)
	if label != "Attempt 2, discarded, no improvement, 95 ms" {
		t.Fatalf("the dot is named %q", label)
	}
}

func TestBrowserClimbPanelFollowsClimbEventsWithoutAReload(t *testing.T) {
	t.Parallel()
	p := openClimbPage(t, "lower")
	p.run(chromedp.Evaluate(`window.__sameDocument = 'yes'`, nil))
	progress := strings.Replace(climbFixture(t, "climb_progress_lower"), `"words": "measuring 2/3"`, `"words": "reviewing"`, 1)
	progress = strings.Replace(progress, `"phase": "measuring"`, `"phase": "reviewing"`, 1)
	progress = strings.Replace(progress, `"summary": "6 attempts · attempt 7 measuring 2/3 · best −15.0%"`, `"summary": "6 attempts · attempt 7 reviewing · best −15.0%"`, 1)
	if progress == climbFixture(t, "climb_progress_lower") {
		t.Fatal("the fixture was not changed")
	}
	p.api.mu.Lock()
	p.api.climb["progress"] = progress
	p.api.mu.Unlock()
	p.api.pushEvent("climb.phase")
	p.await("the new phase", `document.querySelector('#climb-panel [data-field=climb-running] [data-field=phase]')?.textContent === 'reviewing'`)

	attempts := climbFixture(t, "climb_attempts_lower")
	progress = strings.Replace(progress, `"used": 6,`, `"used": 7,`, 1)
	progress = strings.Replace(progress, `"running": {`, `"was_running": {`, 1)
	p.api.mu.Lock()
	p.api.climb["progress"], p.api.climb["attempts"] = progress, attempts
	p.api.mu.Unlock()
	p.api.pushEvent("climb.attempt-recorded")
	p.await("the attempt meter", `document.querySelector('#climb-panel [data-meter=attempts] [data-field=value]')?.textContent === '7 of 20' && document.querySelector('#climb-panel [data-field=climb-running][data-idle=true]') !== null`)
	var same string
	p.eval(`window.__sameDocument`, &same)
	if same != "yes" {
		t.Fatal("the page reloaded")
	}
}

func TestBrowserClimbWorkstreamListShowsTheSummaryAndSparkline(t *testing.T) {
	t.Parallel()
	p := openClimbPage(t, "lower")
	var row struct {
		Summary string
		Points  int
		Spark   bool
	}
	p.eval(`(() => {
  const r = document.querySelector('#workstream-list [data-select="w_000"] [data-field=progress]');
  return { summary: r.querySelector('[data-field=summary]').textContent, points: r.querySelector('polyline')?.getAttribute('points').split(' ').length ?? 0, spark: r.querySelector('svg.spark') !== null && r.querySelector('svg').getAttribute('aria-hidden') === 'true' };
 })()`, &row)
	if row.Summary != "6 attempts · attempt 7 measuring 2/3 · best −15.0%" || !row.Spark || row.Points != 7 {
		t.Fatalf("list row %+v", row)
	}
}

func TestBrowserFeatureWorkstreamStillShowsItsUnitsAndNoClimbPanel(t *testing.T) {
	t.Parallel()
	p, _ := openTubePage(t, nestUnits)
	var shown struct {
		Cells         int
		PanelHidden   bool
		NoClimbLine   bool
		SummaryInList bool
	}
	p.eval(`({
  cells: document.querySelectorAll('#workstream-head [data-field=units] [data-unit-state]').length,
  panelHidden: document.getElementById('climb-panel').hidden && document.getElementById('climb-panel').childElementCount === 0,
  noClimbLine: document.querySelector('#workstream-head [data-field=climb-summary]') === null,
  summaryInList: document.querySelector('#workstream-list [data-field=progress]')?.hidden === false,
 })`, &shown)
	if shown.Cells != 5 || !shown.PanelHidden || !shown.NoClimbLine || shown.SummaryInList {
		t.Fatalf("feature workstream: %+v", shown)
	}
}

func TestBrowserClimbPanelBreaksNoContentSecurityRule(t *testing.T) {
	t.Parallel()
	p := openClimbPage(t, "lower")
	p.click(`#climb-panel g.dot[data-attempt="1"] circle.dot-hit`)
	p.click(`#climb-panel [data-x=time]`)
	var violations []string
	p.eval(`window.__violations`, &violations)
	if len(violations) != 0 {
		t.Fatalf("content-security-policy violations: %q", violations)
	}
	var external int
	p.eval(`document.querySelectorAll('script[src], link[href], img[src]').length - document.querySelectorAll('script[src^="/"], link[href^="/"], img[src^="/"]').length`, &external)
	if external != 0 {
		t.Fatalf("%d resources load from outside the page's own origin", external)
	}
}

func TestBrowserClimbPanelExplainsAClimbThatHasNotStarted(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(1)
	api.mu.Lock()
	api.climbExtra = `,"work_kind":"hill-climb","progress":{"panel":"climb","summary":""}`
	api.mu.Unlock()
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 900), chromedp.Navigate(server.URL+"/"))
	p.click(`[data-select="w_000"]`)
	p.await("the not started note", `document.querySelector('#climb-panel [data-state=error]')?.textContent.includes('The climb has not started yet.')`)
	var problems bool
	p.eval(`!document.getElementById('problems').hidden`, &problems)
	if problems {
		t.Fatal("an unsealed climb raised a problem banner")
	}
}
