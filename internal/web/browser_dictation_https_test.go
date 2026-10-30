package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/chromedp/cdproto/emulation"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/api"
)

func TestBrowserDictationHTTPSAvailability(t *testing.T) {
	t.Parallel()
	for _, phone := range []bool{false, true} {
		t.Run(fmt.Sprintf("phone=%t", phone), func(t *testing.T) {
			t.Parallel()
			const node = "osmia.tail0.ts.net"
			const address = "https://" + node + "/"
			a := newChatAPI()
			handler := a.handler()
			var mu sync.Mutex
			tailnet := &api.TailnetStatus{State: api.TailnetUp, HTTPSState: api.TailnetHTTPSUp, HTTPSAddress: address}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/status" {
					handler.ServeHTTP(w, r)
					return
				}
				recorded := httptest.NewRecorder()
				handler.ServeHTTP(recorded, r)
				var status map[string]any
				if err := json.Unmarshal(recorded.Body.Bytes(), &status); err != nil {
					panic(err)
				}
				mu.Lock()
				status["tailnet"] = tailnet
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(status)
			}))
			t.Cleanup(server.Close)
			options := []chromedp.ExecAllocatorOption{
				chromedp.Flag("host-resolver-rules", "MAP "+node+" 127.0.0.1"),
				chromedp.Flag("no-proxy-server", true),
			}
			if !phone {
				options = append(options, chromedp.Flag("blink-settings", "primaryPointerType=4,availablePointerTypes=4"))
			}
			p := openBrowser(t, options...)
			p.run(chromedp.ActionFunc(func(ctx context.Context) error {
				_, err := cdppage.AddScriptToEvaluateOnNewDocument(fakeDictationScript).Do(ctx)
				return err
			}))
			if phone {
				p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), emulation.SetTouchEmulationEnabled(true))
			} else {
				p.run(emulation.SetDeviceMetricsOverride(1440, 900, 1, false))
			}
			_, port, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			p.run(chromedp.Navigate("http://" + node + ":" + port + "/?source=dictation"))
			p.await("conversation list", `document.querySelectorAll('#workstream-list [data-select]').length === 5`)
			assertDictation(p, `!isSecureContext && location.protocol === 'http:'`)
			for _, state := range []struct {
				tailnet *api.TailnetStatus
				status  string
				link    bool
			}{
				{tailnet, address, true},
				{&api.TailnetStatus{State: api.TailnetUp, HTTPSState: api.TailnetHTTPSOff, HTTPSReason: "disabled by listen.tailnet_https"}, "off — disabled by listen.tailnet_https", false},
				{&api.TailnetStatus{State: api.TailnetUp, HTTPSState: api.TailnetHTTPSUnavailable, HTTPSReason: "enable HTTPS certificates in the tailnet admin console"}, "unavailable — enable HTTPS certificates in the tailnet admin console", false},
				{&api.TailnetStatus{State: api.TailnetUp, HTTPSState: api.TailnetHTTPSUp}, "Tailnet HTTPS: up", false},
				{nil, "", false},
				{tailnet, address, true},
			} {
				mu.Lock()
				tailnet = state.tailnet
				mu.Unlock()
				a.base.pushEvent("resync")
				if state.tailnet == nil {
					p.await("no tailnet status", `document.querySelector('#tailnet-https-status').hidden`)
				} else {
					p.awaitText("#tailnet-https-status", state.status)
					p.click("#settings-button")
					p.await("visible HTTPS status", `document.querySelector('#tailnet-https-status').getBoundingClientRect().width > 0`)
					p.click("#settings-button")
				}
				for _, view := range chatViews {
					// Keep the conversation hash while exercising a non-root path.
					p.eval(`history.replaceState(null, '', '/conversations?source=dictation' + location.hash)`, nil)
					p.selectChat(view, phone)
					destination := `!link.hasAttribute('href')`
					if state.link {
						destination = `link.href === ` + quote(address) + `.replace(/\/$/,'') + location.pathname + location.search + location.hash && link.parentElement.matches('.actions') && link.getBoundingClientRect().width > 0`
					}
					p.await("HTTPS availability in "+view.name, `(() => {
					 const f = document.querySelector(`+quote(view.form)+`), link = f.querySelector('a.dictation-https'), mic = f.querySelector('[aria-label=Dictate]');
					 return mic.hidden && getComputedStyle(mic).display === 'none' && `+fmt.Sprintf(`link.hidden === %t`, !state.link)+` && `+destination+`;
					})()`)
				}
			}
			p.run(chromedp.Navigate(server.URL + "/"))
			p.await("localhost conversations", `document.querySelectorAll('#workstream-list [data-select]').length === 5`)
			assertDictation(p, `isSecureContext`)
			for _, view := range chatViews {
				p.selectChat(view, phone)
				checkDictationIcon(p, view.form, false)
				assertDictation(p, `document.querySelector(`+quote(view.form+" a.dictation-https")+`).hidden`)
			}
			if a.count() != 0 {
				t.Fatal("availability checks sent a message")
			}
		})
	}
}
