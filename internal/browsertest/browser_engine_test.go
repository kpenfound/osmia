package browsertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// Serial: the fake executable directory is installed on the process PATH.
func engineWebFixture(t *testing.T, mode string) (*Service, fixturekit.EngineCLIs) {
	t.Helper()
	fake := fixturekit.InstallEngineCLIs(t)
	must(t, os.WriteFile(fake.Version, []byte("v0.19.0"), 0600))
	opts, _ := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	data = append(data, []byte(fmt.Sprintf("\n[dagger]\nengine = %q\nversion = 'v0.20.5'\ncloud_token_env = 'OSMIA_BROWSER_ENGINE_TOKEN'\n", mode))...)
	must(t, os.WriteFile(path, data, 0600))
	s, _ := start(t, opts)
	return s, fake
}

func openEngineMenu(t *testing.T, s *Service) *page {
	t.Helper()
	p := openOutcomePage(t, s)
	p.Click("#settings-button")
	p.AwaitText("#engine-status [data-field=mode]", "managed")
	return p
}

func TestBrowserEngineControls(t *testing.T) {
	s, fake := engineWebFixture(t, "managed")
	release, err := service.InspectForTest(s).AcquireCheckSlot(context.Background(), service.CheckSlotRun{Project: project, Workstream: stream, Unit: "upload"})
	must(t, err)
	defer release()
	p := openEngineMenu(t, s)
	for field, want := range map[string]string{
		"reachable": "Reachable", "version": "v0.19.0", "expected-version": "v0.20.5", "cli-version": "v0.20.5", "mismatch": "Engine differs from host CLI", "cache-size": "4,096 bytes",
	} {
		p.AwaitText("#engine-status [data-field="+field+"]", want)
	}
	before := fake.Commands(t)
	for _, action := range []string{"restart", "prune"} {
		p.Click("#engine-" + action)
		selector := "#engine-" + action + "-result[data-outcome=unavailable]"
		p.AwaitText(selector, "temporarily unavailable")
		p.AwaitText(selector, "unit upload")
		p.AwaitText(selector, string(stream))
	}
	if got := fake.Commands(t); got != before {
		t.Fatalf("busy actions touched Docker: %s", got)
	}
	release()
	p.Click("#engine-restart")
	assertOutcome(p, "#engine-restart-result", "ok", "Engine restarted.")
	p.AwaitText("#engine-status [data-field=version]", "v0.20.5")
	p.AwaitText("#engine-status [data-field=mismatch]", "None")
	for _, command := range []string{"stop osmia-dagger-engine", "rm osmia-dagger-engine", "osmia-dagger-engine-cache:/var/lib/dagger registry.dagger.io/engine:v0.20.5"} {
		if !strings.Contains(fake.Commands(t), command) {
			t.Fatalf("restart missing %s: %s", command, fake.Commands(t))
		}
	}
	p.Click("#engine-prune")
	assertOutcome(p, "#engine-prune-result", "ok", "Engine cache pruned.")
	// The menu remains within the phone viewport after status has expanded it.
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile))
	p.Await("the cog menu fits the phone", `(() => { const r = document.getElementById('settings-menu').getBoundingClientRect(); return r.left >= 0 && r.right <= innerWidth && r.bottom <= innerHeight; })()`)
}

func TestBrowserEngineNonManagedRefusals(t *testing.T) {
	for _, mode := range []string{"auto", "tcp://external-engine:1234", "cloud"} {
		t.Run(mode, func(t *testing.T) {
			s, fake := engineWebFixture(t, mode)
			p := openOutcomePage(t, s)
			p.Click("#settings-button")
			expected := mode
			if strings.HasPrefix(mode, "tcp:") {
				expected = "address"
			}
			p.AwaitText("#engine-status [data-field=mode]", expected)
			if mode == "cloud" {
				p.AwaitText("#engine-status [data-field=version]", "Unknown")
				p.AwaitText("#engine-status [data-field=reachable]", "Unreachable")
				p.AwaitText("#engine-status [data-field=reachability-error]", "configured Cloud token is not set")
				p.AwaitText("#engine-status [data-field=cache-size]", "Unknown")
			} else {
				p.AwaitText("#engine-status [data-field=version]", "v0.19.0")
			}
			for _, action := range []string{"restart", "prune"} {
				p.Click("#engine-" + action)
				p.AwaitText("#engine-"+action+"-result[data-outcome=error]", "not applicable to this mode ("+expected+")")
			}
			if got := fake.Commands(t); got != "" {
				t.Fatalf("non-managed actions touched Docker: %s", got)
			}
		})
	}
}

func TestBrowserEngineOutcomesBelongToMenuOpening(t *testing.T) {
	s, _ := engineWebFixture(t, "managed")
	p := openEngineMenu(t, s)
	for _, action := range []string{"restart", "prune"} {
		url := "/v1/engine/" + action
		holdOutcome(p, url)
		p.Click("#engine-" + action)
		p.Await("the maintenance response is held", `typeof releaseOutcome[`+quote(url)+`] === 'function'`)
		assertOutcome(p, "#engine-"+action+"-result", "pending", "Sending…")
		p.Click("#settings-button")
		p.Click("#settings-button")
		assertOutcome(p, "#engine-"+action+"-result", "", "")
		p.Eval(`window.releaseOldEngineOutcome = releaseOutcome[`+quote(url)+`]`, nil)
		holdOutcome(p, url)
		p.Click("#engine-" + action)
		p.Await("the new opening's response is held", `typeof releaseOutcome[`+quote(url)+`] === 'function'`)
		p.Eval(`releaseOldEngineOutcome()`, nil)
		// Complete another request after the release to cross the old callback.
		p.Eval(`fetch('/v1/engine/status').then(() => window.engineOutcomeSettled = true)`, nil)
		p.Await("the late action settled", `window.engineOutcomeSettled === true`)
		assertOutcome(p, "#engine-"+action+"-result", "pending", "Sending…")
		releaseOutcome(p, url)
		want := "Engine restarted."
		if action == "prune" {
			want = "Engine cache pruned."
		}
		assertOutcome(p, "#engine-"+action+"-result", "ok", want)
		p.Eval(`window.engineOutcomeSettled = false`, nil)
	}
}
