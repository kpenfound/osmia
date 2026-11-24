package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
)

type settingsAPI struct {
	*fakeAPI

	mu       sync.Mutex
	pin      string
	timeout  string
	landing  string
	inEffect string
	unloaded bool
	saves    []settingsSave
}

type settingsSave struct {
	Pin string                     `json:"pin"`
	Set map[string]json.RawMessage `json:"set"`
}

func (a *settingsAPI) snapshot() map[string]any {
	setting := func(name string, value, effective any, isDefault bool) map[string]any {
		return map[string]any{"name": name, "value": value, "default": isDefault, "effective": effective, "pending": !reflect.DeepEqual(value, effective)}
	}
	if a.unloaded {
		effective := func(name string, value any) map[string]any {
			return map[string]any{"name": name, "effective": value, "pending": false}
		}
		failure := map[string]any{"field": "clone", "message": "project p_fake settings: clone: clone is a regular file, not a directory"}
		settings := []any{effective("upstream", "owner/shop"), effective("checks_timeout", a.inEffect)}
		return map[string]any{"project": "p_fake", "revision": 1, "error": failure, "settings": settings}
	}
	return map[string]any{"project": "p_fake", "revision": len(a.saves) + 1, "pin": a.pin, "settings": []any{
		setting("upstream", "owner/shop", "owner/shop", false),
		setting("landing", a.landing, "commit-per-unit", false),
		setting("checks_timeout", a.timeout, a.inEffect, false),
		setting("hearsay_entities", map[string]string{}, map[string]string{}, true),
	}}
}

func (a *settingsAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/config", handleJSON(`{"root":"","digest":"fake","effective":{"profiles":{}},"project":null,`+
		`"projects":[{"id":"p_fake","name":"shop","upstream":"owner/shop","clone":"/clone","base_branch":"main","trace":"","repos":[{"id":"p_fake","name":"shop","upstream":"owner/shop","clone":"/clone","base_branch":"main","landing":"commit-per-unit","trace":"","kb":"","entity_map":""}]}],`+
		`"diagnostics":[],"drift":{"differs":false,"files":[]}}`))
	mux.HandleFunc("/v1/projects/charter/p_fake", handleJSON(`{"project":"p_fake","revision":1,"content":""}`))
	mux.HandleFunc("/v1/projects/config/p_fake", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if r.Method == http.MethodGet {
			body, _ := json.Marshal(a.snapshot())
			writeJSON(w, string(body))
			return
		}
		data, _ := io.ReadAll(r.Body)
		var save settingsSave
		_ = json.Unmarshal(data, &save)
		a.saves = append(a.saves, save)
		if save.Pin != a.pin {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, `{"error":{"code":"conflict","message":"changed since it was read"}}`)
			return
		}
		applied := len(a.saves) > 1
		for name, raw := range save.Set {
			var value string
			_ = json.Unmarshal(raw, &value)
			switch name {
			case "checks_timeout":
				a.timeout = value
			case "landing":
				a.landing = value
			}
		}
		a.pin = "pin-" + string(rune('0'+len(a.saves)+1))
		outcome, reason := "applied", ""
		if applied {
			a.inEffect = a.timeout
		} else {
			outcome, reason = "saved_not_applied", "the stages could not be prepared."
		}
		saved := a.snapshot()
		saved["outcome"] = outcome
		if reason != "" {
			saved["reason"] = reason
		}
		body, _ := json.Marshal(saved)
		writeJSON(w, string(body))
	})
	mux.Handle("/", a.fakeAPI.handler())
	return mux
}

func (p *page) openProjectSettings(url string) {
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(url+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.click("#settings-button")
	p.click(`#settings-nav [data-open=projects]`)
	p.await("the project to manage", `document.querySelector('#project-edit [name=project]').options.length > 1`)
	p.run(chromedp.SetValue("#project-edit [name=project]", "p_fake", chromedp.ByQuery))
	p.eval(`document.querySelector('#project-edit [name=project]').dispatchEvent(new Event('change', {bubbles: true}))`, nil)
}

func (a *settingsAPI) seen() []settingsSave {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]settingsSave(nil), a.saves...)
}

type savedSettingsPage struct {
	p   *page
	api *settingsAPI
}

func settingField(name string) string { return `#project-config [data-setting="` + name + `"] ` }

func openSettingsPage(t *testing.T) savedSettingsPage {
	t.Helper()
	api := &settingsAPI{fakeAPI: newFakeAPI(0), pin: "pin-1", timeout: "15m", landing: "commit-per-unit", inEffect: "15m"}
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.openProjectSettings(server.URL)
	p.awaitText("#config-file", "Stored settings of project p_fake, revision 1")
	p.await("the stored value", `document.querySelector(`+quote(settingField("checks_timeout")+"input")+`).value === '15m'`)
	return savedSettingsPage{p: p, api: api}
}

func (s savedSettingsPage) saveTimeout() {
	s.p.eval(`document.querySelector(`+quote(settingField("checks_timeout")+"input")+`).value = '30m'`, nil)
	s.p.click("#project-config [type=submit]")
	s.p.await("the save shown as saved but not applied", `document.querySelector('#project-config .result').dataset.outcome === 'saved-not-applied'`)
}

func (s savedSettingsPage) saveLanding() {
	s.p.eval(`document.querySelector(`+quote(settingField("landing")+"input")+`).value = 'squash'`, nil)
	s.p.click("#project-config [type=submit]")
	s.p.await("the later save accepted", `document.querySelector('#project-config .result').dataset.outcome === 'ok'`)
}

func TestBrowserProjectSettingsShowASavedButNotAppliedOutcome(t *testing.T) {
	t.Parallel()
	s := openSettingsPage(t)
	s.saveTimeout()
	s.p.awaitText("#project-config .result", "the stages could not be prepared.")
	s.p.awaitText("#project-config .result", "next save or a restart")
}

func TestBrowserProjectSettingsMarkTheSavedValueAsPending(t *testing.T) {
	t.Parallel()
	s := openSettingsPage(t)
	s.saveTimeout()
	s.p.awaitText(settingField("checks_timeout")+`[data-marker=pending]`, "Not yet applied. In effect: 15m")
}

func TestBrowserProjectSettingsKeepTheSavedValueAndAdoptTheReturnedPin(t *testing.T) {
	t.Parallel()
	s := openSettingsPage(t)
	s.saveTimeout()
	s.p.await("the saved value kept and the returned pin adopted", `document.querySelector(`+quote(settingField("checks_timeout")+"input")+`).value === '30m' && document.querySelector('#project-config').dataset.pin === 'pin-2'`)
	s.p.await("no stale notice", `document.querySelector('#config-changed').hidden`)
}

func TestBrowserProjectSettingsPinALaterSaveToTheReturnedPin(t *testing.T) {
	t.Parallel()
	s := openSettingsPage(t)
	s.saveTimeout()
	s.p.await("the returned pin adopted", `document.querySelector('#project-config').dataset.pin === 'pin-2'`)
	s.saveLanding()
	saves := s.api.seen()
	if len(saves) != 2 || saves[0].Pin != "pin-1" || saves[1].Pin != "pin-2" {
		t.Fatalf("saves were pinned to %+v, want the first read's pin and then the returned one", saves)
	}
}

func TestBrowserProjectSettingsSendOnlyTheChangedSetting(t *testing.T) {
	t.Parallel()
	s := openSettingsPage(t)
	s.saveTimeout()
	s.p.await("the returned pin adopted", `document.querySelector('#project-config').dataset.pin === 'pin-2'`)
	s.saveLanding()
	saves := s.api.seen()
	if len(saves) != 2 || len(saves[1].Set) != 1 || strings.TrimSpace(string(saves[1].Set["landing"])) != `"squash"` {
		t.Fatalf("the later save sent more than the changed setting: %+v", saves)
	}
}

func TestBrowserProjectSettingsClearThePendingMarkerOnceApplied(t *testing.T) {
	t.Parallel()
	s := openSettingsPage(t)
	s.saveTimeout()
	s.p.await("the returned pin adopted", `document.querySelector('#project-config').dataset.pin === 'pin-2'`)
	s.saveLanding()
	s.p.await("the saved values applied", `!document.querySelector(`+quote(settingField("checks_timeout")+"[data-marker=pending]")+`).textContent`)
}

func TestBrowserProjectSettingsNameTheStoredSettingsNotAFile(t *testing.T) {
	t.Parallel()
	api := &settingsAPI{fakeAPI: newFakeAPI(0), pin: "pin-1", timeout: "15m", landing: "commit-per-unit", inEffect: "15m"}
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.openProjectSettings(server.URL)
	p.awaitText("#config-file", "Stored settings of project p_fake, revision 1")
	var text string
	p.eval(`document.querySelector('#project-config').textContent`, &text)
	for _, banned := range []string{"config.toml", "edit the file", "by hand", "hand-edit"} {
		if strings.Contains(text, banned) {
			t.Fatalf("the settings form says %q:\n%s", banned, text)
		}
	}
}

func TestBrowserProjectSettingsSurfaceAConflictAndKeepTheDraft(t *testing.T) {
	t.Parallel()
	api := &settingsAPI{fakeAPI: newFakeAPI(0), pin: "pin-1", timeout: "15m", landing: "commit-per-unit", inEffect: "15m"}
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.openProjectSettings(server.URL)
	field := func(name string) string { return `#project-config [data-setting="` + name + `"] ` }
	p.awaitText("#config-file", "Stored settings of project p_fake, revision 1")
	p.await("the stored value", `document.querySelector(`+quote(field("landing")+"input")+`).value === 'commit-per-unit'`)

	api.mu.Lock()
	api.pin, api.landing = "pin-elsewhere", "squash"
	api.mu.Unlock()
	p.eval(`document.querySelector(`+quote(field("checks_timeout")+"input")+`).value = '45m'`, nil)
	p.click("#project-config [type=submit]")
	p.await("the save refused as stale", `document.querySelector('#project-config .result').dataset.outcome === 'error'`)
	p.awaitText("#project-config .result", "changed since it was read")
	p.await("the notice that the settings changed", `!document.querySelector('#config-changed').hidden`)
	p.await("the draft and its pin kept", `document.querySelector(`+quote(field("checks_timeout")+"input")+`).value === '45m' && document.querySelector('#project-config').dataset.pin === 'pin-1'`)

	p.click("#project-config [data-action=read-config]")
	p.await("the stored values loaded", `document.querySelector(`+quote(field("landing")+"input")+`).value === 'squash' && document.querySelector('#project-config').dataset.pin === 'pin-elsewhere'`)
	p.await("the notice cleared", `document.querySelector('#config-changed').hidden`)
	p.eval(`document.querySelector(`+quote(field("checks_timeout")+"input")+`).value = '45m'`, nil)
	p.click("#project-config [type=submit]")
	p.await("the save from the latest read accepted", `document.querySelector('#project-config .result').dataset.outcome === 'ok'`)
	if saves := api.seen(); len(saves) != 2 || saves[0].Pin != "pin-1" || saves[1].Pin != "pin-elsewhere" {
		t.Fatalf("saves were pinned to %+v, want the stale pin and then the one read afterwards", saves)
	}
}

func TestBrowserProjectSettingsThatDoNotLoadPointToTheRepairCommand(t *testing.T) {
	t.Parallel()
	api := &settingsAPI{fakeAPI: newFakeAPI(0), unloaded: true, inEffect: "15m"}
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.openProjectSettings(server.URL)
	p.awaitText("#config-file", "read-only: they do not load")
	p.awaitText("#config-load-error", "clone is a regular file")
	p.awaitText("#config-load-error", "osmia project settings p_fake set clone=VALUE while the service is stopped")
	p.await("no save offered", `document.querySelector('#project-config [type=submit]').disabled`)
	var text string
	p.eval(`document.querySelector('#project-config').textContent`, &text)
	if strings.Contains(text, "config.toml") || strings.Contains(text, "by hand") {
		t.Fatalf("the form tells the owner to edit a file:\n%s", text)
	}
}
