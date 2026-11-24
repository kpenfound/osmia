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

const (
	tailnetNode    = "osmia.tail0.ts.net"
	tailnetAddress = "https://" + tailnetNode + "/"
)

type tailnetPage struct {
	p         *page
	a         *chatAPI
	phone     bool
	serverURL string
	mu        sync.Mutex
	tailnet   *api.TailnetStatus
}

func openTailnetPage(t *testing.T, phone bool) *tailnetPage {
	t.Helper()
	tp := &tailnetPage{a: newChatAPI(), phone: phone,
		tailnet: &api.TailnetStatus{State: api.TailnetUp, HTTPSState: api.TailnetHTTPSUp, HTTPSAddress: tailnetAddress}}
	handler := tp.a.handler()
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
		tp.mu.Lock()
		status["tailnet"] = tp.tailnet
		tp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	}))
	t.Cleanup(server.Close)
	tp.serverURL = server.URL
	options := []chromedp.ExecAllocatorOption{
		chromedp.Flag("host-resolver-rules", "MAP "+tailnetNode+" 127.0.0.1"),
		chromedp.Flag("no-proxy-server", true),
	}
	if !phone {
		options = append(options, chromedp.Flag("blink-settings", "primaryPointerType=4,availablePointerTypes=4"))
	}
	tp.p = openBrowser(t, options...)
	tp.p.run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(fakeDictationScript).Do(ctx)
		return err
	}))
	if phone {
		tp.p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), emulation.SetTouchEmulationEnabled(true))
	} else {
		tp.p.run(emulation.SetDeviceMetricsOverride(1440, 900, 1, false))
	}
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tp.p.run(chromedp.Navigate("http://" + tailnetNode + ":" + port + "/?source=dictation"))
	tp.p.await("conversation list", `document.querySelectorAll('#workstream-list [data-select]').length === 5`)
	assertDictation(tp.p, `!isSecureContext && location.protocol === 'http:'`)
	return tp
}

func (tp *tailnetPage) setTailnet(status *api.TailnetStatus) {
	tp.mu.Lock()
	tp.tailnet = status
	tp.mu.Unlock()
	tp.a.base.pushEvent("resync")
}

func (tp *tailnetPage) awaitStatusText(text string) {
	tp.p.awaitText("#tailnet-https-status", text)
	tp.p.click("#settings-button")
	tp.p.await("visible HTTPS status", `document.querySelector('#tailnet-https-status').getBoundingClientRect().width > 0`)
	tp.p.click("#settings-button")
}

func (tp *tailnetPage) awaitDictationLinks(link bool) {
	for _, view := range chatViews {
		tp.p.eval(`history.replaceState(null, '', '/conversations?source=dictation' + location.hash)`, nil)
		tp.p.selectChat(view, tp.phone)
		destination := `!link.hasAttribute('href')`
		if link {
			destination = `link.href === ` + quote(tailnetAddress) + `.replace(/\/$/,'') + location.pathname + location.search + location.hash && link.parentElement.matches('.actions') && link.getBoundingClientRect().width > 0`
		}
		tp.p.await("HTTPS availability in "+view.name, `(() => {
		 const f = document.querySelector(`+quote(view.form)+`), link = f.querySelector('a.dictation-https'), mic = f.querySelector('[aria-label=Dictate]');
		 return mic.hidden && getComputedStyle(mic).display === 'none' && `+fmt.Sprintf(`link.hidden === %t`, !link)+` && `+destination+`;
		})()`)
	}
}

func forEachTailnetPage(t *testing.T, check func(tp *tailnetPage)) {
	t.Helper()
	for _, phone := range []bool{false, true} {
		t.Run(fmt.Sprintf("phone=%t", phone), func(t *testing.T) {
			t.Parallel()
			check(openTailnetPage(t, phone))
		})
	}
}

func TestBrowserDictationLinksToTheHTTPSAddressOnAnInsecureOrigin(t *testing.T) {
	t.Parallel()
	forEachTailnetPage(t, func(tp *tailnetPage) {
		tp.awaitStatusText(tailnetAddress)
		tp.awaitDictationLinks(true)
	})
}

func TestBrowserDictationExplainsWhyTailnetHTTPSIsNotOffered(t *testing.T) {
	t.Parallel()
	forEachTailnetPage(t, func(tp *tailnetPage) {
		for _, state := range []struct {
			tailnet *api.TailnetStatus
			status  string
		}{
			{&api.TailnetStatus{State: api.TailnetUp, HTTPSState: api.TailnetHTTPSOff, HTTPSReason: "disabled by listen.tailnet_https"}, "off — disabled by listen.tailnet_https"},
			{&api.TailnetStatus{State: api.TailnetUp, HTTPSState: api.TailnetHTTPSUnavailable, HTTPSReason: "enable HTTPS certificates in the tailnet admin console"}, "unavailable — enable HTTPS certificates in the tailnet admin console"},
			{&api.TailnetStatus{State: api.TailnetUp, HTTPSState: api.TailnetHTTPSUp}, "Tailnet HTTPS: up"},
		} {
			tp.setTailnet(state.tailnet)
			tp.awaitStatusText(state.status)
			tp.awaitDictationLinks(false)
		}
	})
}

func TestBrowserDictationHidesTheHTTPSStatusWithoutATailnet(t *testing.T) {
	t.Parallel()
	forEachTailnetPage(t, func(tp *tailnetPage) {
		tp.setTailnet(nil)
		tp.p.await("no tailnet status", `document.querySelector('#tailnet-https-status').hidden`)
		tp.awaitDictationLinks(false)
	})
}

func TestBrowserDictationFollowsTheTailnetHTTPSStatusWhenItChanges(t *testing.T) {
	t.Parallel()
	forEachTailnetPage(t, func(tp *tailnetPage) {
		tp.setTailnet(&api.TailnetStatus{State: api.TailnetUp, HTTPSState: api.TailnetHTTPSOff, HTTPSReason: "disabled by listen.tailnet_https"})
		tp.awaitStatusText("off — disabled by listen.tailnet_https")
		tp.awaitDictationLinks(false)
		tp.setTailnet(&api.TailnetStatus{State: api.TailnetUp, HTTPSState: api.TailnetHTTPSUp, HTTPSAddress: tailnetAddress})
		tp.awaitStatusText(tailnetAddress)
		tp.awaitDictationLinks(true)
	})
}

func TestBrowserDictationOnASecureOriginShowsTheMicrophoneAndNoLink(t *testing.T) {
	t.Parallel()
	forEachTailnetPage(t, func(tp *tailnetPage) {
		tp.p.run(chromedp.Navigate(tp.serverURL + "/"))
		tp.p.await("localhost conversations", `document.querySelectorAll('#workstream-list [data-select]').length === 5`)
		assertDictation(tp.p, `isSecureContext`)
		for _, view := range chatViews {
			tp.p.selectChat(view, tp.phone)
			checkDictationIcon(tp.p, view.form, false)
			assertDictation(tp.p, `document.querySelector(`+quote(view.form+" a.dictation-https")+`).hidden`)
		}
	})
}
