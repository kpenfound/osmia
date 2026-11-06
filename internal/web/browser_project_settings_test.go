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

// settingsAPI answers Project management's project settings the way the
// service does, for one project whose file the test controls: a snapshot of
// the file with a pin, and a save that is pinned to it. The first save
// replaces the file but cannot apply it, so the snapshot after it shows the
// saved value as not yet applied; later saves apply.
type settingsAPI struct {
	*fakeAPI

	mu       sync.Mutex
	pin      string
	timeout  string // checks_timeout in the file
	landing  string // landing in the file
	inEffect string // checks_timeout in the loaded configuration
	saves    []settingsSave
}

type settingsSave struct {
	Pin string                     `json:"pin"`
	Set map[string]json.RawMessage `json:"set"`
}

// snapshot is the project's file as the read reports it.
func (a *settingsAPI) snapshot() map[string]any {
	setting := func(name string, value, effective any, isDefault bool) map[string]any {
		return map[string]any{"name": name, "value": value, "default": isDefault, "effective": effective, "pending": !reflect.DeepEqual(value, effective)}
	}
	return map[string]any{"project": "p_fake", "path": "/root/projects/p_fake/config.toml", "pin": a.pin, "settings": []any{
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

func (a *settingsAPI) seen() []settingsSave {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]settingsSave(nil), a.saves...)
}

// A save the service reports as saved but not applied is shown as saved and
// pending, not as a refusal. The form takes the returned pin, shows the saved
// value as not yet applied beside the value in effect, and a later save
// from it is accepted rather than refused as stale.
func TestBrowserProjectSettingsShowASavedButNotAppliedOutcomeAndAdoptItsPin(t *testing.T) {
	api := &settingsAPI{fakeAPI: newFakeAPI(0), pin: "pin-1", timeout: "15m", landing: "commit-per-unit", inEffect: "15m"}
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.eval(`document.querySelector('#settings-menu [data-open=projects]').click()`, nil)
	p.await("the project to manage", `document.querySelector('#project-edit [name=project]').options.length > 1`)
	p.run(chromedp.SetValue("#project-edit [name=project]", "p_fake", chromedp.ByQuery))
	p.eval(`document.querySelector('#project-edit [name=project]').dispatchEvent(new Event('change', {bubbles: true}))`, nil)
	field := func(name string) string { return `#project-config [data-setting="` + name + `"] ` }
	p.awaitText("#config-file", "/root/projects/p_fake/config.toml")
	p.await("the file's value", `document.querySelector(`+quote(field("checks_timeout")+"input")+`).value === '15m'`)

	p.eval(`document.querySelector(`+quote(field("checks_timeout")+"input")+`).value = '30m'`, nil)
	p.click("#project-config [type=submit]")
	p.await("the save shown as saved but not applied", `document.querySelector('#project-config .result').dataset.outcome === 'saved-not-applied'`)
	p.awaitText("#project-config .result", "the stages could not be prepared.")
	p.awaitText("#project-config .result", "Reload")
	p.awaitText(field("checks_timeout")+`[data-marker=pending]`, "Not yet applied. In effect: 15m")
	p.await("the saved value kept and the returned pin adopted", `document.querySelector(`+quote(field("checks_timeout")+"input")+`).value === '30m' && document.querySelector('#project-config').dataset.pin === 'pin-2'`)
	p.await("no stale notice", `document.querySelector('#config-changed').hidden`)

	p.eval(`document.querySelector(`+quote(field("landing")+"input")+`).value = 'squash'`, nil)
	p.click("#project-config [type=submit]")
	p.await("the later save accepted", `document.querySelector('#project-config .result').dataset.outcome === 'ok'`)
	saves := api.seen()
	if len(saves) != 2 || saves[0].Pin != "pin-1" || saves[1].Pin != "pin-2" {
		t.Fatalf("saves were pinned to %+v, want the first read's pin and then the returned one", saves)
	}
	if len(saves[1].Set) != 1 || strings.TrimSpace(string(saves[1].Set["landing"])) != `"squash"` {
		t.Fatalf("the later save sent more than the changed setting: %+v", saves[1])
	}
	p.await("the saved values applied", `!document.querySelector(`+quote(field("checks_timeout")+"[data-marker=pending]")+`).textContent`)
}
