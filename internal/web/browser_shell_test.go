package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

const luminanceFunction = `const lightness = (c) => {
	let m = c.match(/oklch\(([\d.]+)/);
	if (m) return +m[1];
	m = c.match(/color\(srgb ([\d.]+) ([\d.]+) ([\d.]+)/);
	if (m) return 0.2126 * +m[1] + 0.7152 * +m[2] + 0.0722 * +m[3];
	m = c.match(/rgba?\((\d+)[, ]+(\d+)[, ]+(\d+)/);
	return m ? (0.2126 * +m[1] + 0.7152 * +m[2] + 0.0722 * +m[3]) / 255 : NaN;
};`

func TestBrowserHomeLeadsWithWhatNeedsTheOwner(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(3)
	api.setInbox(ratificationEntry("w_000"), ratificationEntry("w_001"), ratificationEntry("w_002"))
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.await("one entry for each workstream that waits", `document.querySelectorAll('#project-pane .needs-you [data-needs-workstream]').length === 3`)
	p.await("the list first on home, with its count", `(() => {
		const list = document.querySelector('#project-pane > :first-child');
		return list.matches('.needs-you') && list.querySelector('h3').textContent === 'Needs you' &&
			list.querySelector('[data-field=needs-count]').textContent === '3';
	})()`)
	for _, id := range []string{"w_000", "w_001", "w_002"} {
		entry := `#project-pane .needs-you [data-needs-workstream="` + id + `"] a`
		p.await(id+" linked to its workstream", `document.querySelector(`+quote(entry)+`).getAttribute('href') === '#workstream=`+id+`'`)
		p.awaitText(entry+` [data-field=kind]`, "Ratification")
		p.awaitText(entry+` [data-field=goal]`, "Workstream "+id[len(id)-1:])
		p.awaitText(entry+` [data-field=question]`, "Ratify spec.md revision 1")
	}
	p.click(`#project-pane .needs-you [data-needs-workstream="w_001"] a`)
	p.await("the linked workstream", `!document.querySelector('[data-view=workstream]').hidden && document.querySelector('#workstream-head').dataset.workstream === 'w_001' && location.hash === '#workstream=w_001'`)
	p.click("#project-home")
	p.await("home again", `!document.querySelector('[data-view=project-pane]').hidden`)

	api.setInbox()
	api.pushEvent("inbox")
	p.await("the list says nothing waits once the inbox is empty", `document.querySelector('.needs-you [data-field=needs-empty]').hidden === false && document.querySelectorAll('.needs-you [data-needs-workstream]').length === 0`)
}

func TestBrowserSidebarIsQuietAndRowsShowGlyphAndCount(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(2)
	api.setInbox(ratificationEntry("w_000"))
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.await("both rows listed", `document.querySelectorAll('#workstream-list [data-select]').length === 2`)
	p.await("no create button in the sidebar", `document.querySelectorAll('#sidebar [data-open], #sidebar .new').length === 0 &&
		![...document.querySelectorAll('#sidebar button')].some(b => /Hand in|Open a|Open an|Create/.test(b.textContent))`)
	p.awaitText(`[data-select="w_000"] [data-field=needs]`, "1")
	p.await("a state glyph and no needs count where nothing waits", `document.querySelector('[data-select="w_001"] .glyph[data-field=state]').dataset.glyph === 'live' &&
		document.querySelector('[data-select="w_001"] [data-field=needs]') === null`)
	p.await("the glyph is named for assistive technology", `document.querySelector('[data-select="w_000"] .glyph').getAttribute('aria-label') === 'building'`)
	p.await("no kind, project and state text", `(() => {
		const text = document.querySelector('[data-select="w_000"]').textContent;
		return !text.includes('·') && !text.includes('building') && document.querySelector('[data-select="w_000"] [data-field=state]').textContent === '';
	})()`)
	var lower bool
	p.eval(`(() => { `+luminanceFunction+`
		return lightness(getComputedStyle(document.getElementById('sidebar')).backgroundColor) < lightness(getComputedStyle(document.body).backgroundColor);
	})()`, &lower)
	if !lower {
		t.Fatal("the sidebar does not sit on a lower surface step than the main area")
	}
}

func openLivePage(t *testing.T) (*page, *httptest.Server) {
	t.Helper()
	api := newFakeAPI(1)
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	return p, server
}

func TestBrowserHeaderShowsOnlyADotWhileLive(t *testing.T) {
	t.Parallel()
	p, _ := openLivePage(t)
	p.await("only a dot while live", `(() => {
		const dot = document.querySelector('#connection .dot').getBoundingClientRect();
		const label = document.querySelector('#connection .label').getBoundingClientRect();
		return dot.width > 0 && label.width <= 1 && label.height <= 1;
	})()`)
}

func TestBrowserHeaderSaysSoWhenTheConnectionIsLost(t *testing.T) {
	t.Parallel()
	p, server := openLivePage(t)
	server.CloseClientConnections()
	p.await("a clear message once lost", `document.body.dataset.connection === 'lost' && (() => {
		const label = document.querySelector('#connection .label');
		return label.getBoundingClientRect().width > 1 && label.textContent.includes('lost');
	})()`)
}

func TestBrowserHeaderReturnsToTheDotWhenTheStreamIsBack(t *testing.T) {
	t.Parallel()
	p, server := openLivePage(t)
	server.CloseClientConnections()
	p.await("lost", `document.body.dataset.connection === 'lost'`)
	p.await("the dot alone again once the stream is back", `document.body.dataset.connection === 'live' && document.querySelector('#connection .label').getBoundingClientRect().width <= 1`)
}

func TestBrowserHeaderIndicatorsShareOneShape(t *testing.T) {
	t.Parallel()
	p, _ := openLivePage(t)
	p.await("capacity, pauses and activity share one shape", `(() => {
		const shape = (id) => {
			const s = getComputedStyle(document.getElementById(id));
			return [s.height, s.borderRadius, s.fontSize, s.paddingLeft, s.display].join(' ');
		};
		const ids = ['capacity-button', 'pause-button', 'activity-button'];
		return ids.every((id) => document.getElementById(id).classList.contains('chip')) &&
			new Set(ids.map(shape)).size === 1;
	})()`)
}

func openSettingsView(t *testing.T) *page {
	t.Helper()
	p, _ := openLivePage(t)
	return p
}

func TestBrowserSettingsHasNoPopover(t *testing.T) {
	t.Parallel()
	p := openSettingsView(t)
	p.await("no popover plays the settings role", `document.querySelector('#settings-menu[popover], [popover] #engine-restart, [popover] #tailnet-https-status') === null`)
}

func TestBrowserSettingsButtonOpensTheSettingsView(t *testing.T) {
	t.Parallel()
	p := openSettingsView(t)
	p.click("#settings-button")
	p.await("the Settings view", `!document.getElementById('settings-menu').hidden && document.querySelector('#settings-title').textContent === 'Settings'`)
}

func TestBrowserSettingsListsFiveSectionsInOrder(t *testing.T) {
	t.Parallel()
	p := openSettingsView(t)
	p.click("#settings-button")
	var names []string
	p.eval(`[...document.querySelectorAll('#settings-nav [data-open]')].map(b => b.textContent.trim())`, &names)
	want := []string{"Projects", "Priority", "Profiles and usage", "Configuration", "Dagger engine"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("settings sections %v, want %v", names, want)
	}
}

func TestBrowserSettingsShowsOnlyTheChosenSection(t *testing.T) {
	t.Parallel()
	p := openSettingsView(t)
	p.click("#settings-button")
	for _, section := range []string{"projects", "priority", "profiles", "config", "engine"} {
		p.click(`#settings-nav [data-open="` + section + `"]`)
		p.await("only the "+section+" section", `(() => {
			const shown = [...document.querySelectorAll('.settings-section')].filter(s => !s.hidden).map(s => s.dataset.section);
			return shown.length === 1 && shown[0] === `+quote(section)+` &&
				document.querySelector('#settings-nav [data-open="`+section+`"]').getAttribute('aria-current') === 'true';
		})()`)
	}
}

func TestBrowserSettingsPutsRestartAndPruneInTheEngineSection(t *testing.T) {
	t.Parallel()
	p := openSettingsView(t)
	p.click("#settings-button")
	p.click(`#settings-nav [data-open="engine"]`)
	p.await("Restart and Prune in the engine section", `(() => {
		const section = document.getElementById('engine-controls');
		return section.dataset.section === 'engine' && !section.hidden &&
			section.contains(document.getElementById('engine-restart')) && section.contains(document.getElementById('engine-prune'));
	})()`)
}

func TestBrowserSettingsButtonClosesTheSettingsView(t *testing.T) {
	t.Parallel()
	p := openSettingsView(t)
	p.click("#settings-button")
	p.await("the Settings view", `!document.getElementById('settings-menu').hidden`)
	p.click("#settings-button")
	p.await("the Settings view closed", `document.getElementById('settings-menu').hidden && !document.querySelector('[data-view=project-pane]').hidden`)
}
