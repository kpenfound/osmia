package browsertest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// The origin stays the full MagicDNS name at the standard HTTP and HTTPS
// ports. Chromium's resolver maps each port to the fake node's actual listener;
// the SPKI allowlist trusts only that node's test certificate.
func TestBrowserTailnetHTTPSDictationJourney(t *testing.T) {
	t.Parallel()
	binary := os.Getenv("OSMIA_BROWSER")
	if binary == "" {
		t.Skip("OSMIA_BROWSER names no Chromium binary; dagger check runs the browser tests")
	}
	const nodeName = "osmia.tail0.ts.net"
	const secureAddress = "https://" + nodeName + "/"
	opts, _ := servicetest.ConversationFixture(t, "dictation-https-")
	fixturekit.WithListen(t, opts.Config.Root, "tailnet = \"osmia\"\n")
	join, joined := fixturekit.TailnetJoiner(nodeName, "osmia")
	opts.JoinTailnet = func(host, dir string) (service.TailnetListener, error) { return join(host, dir) }
	s, c := servicetest.Start(t, opts)
	deadline := time.Now().Add(BrowserTimeout)
	for {
		h, err := c.Health(context.Background())
		must(t, err)
		if h.Tailnet != nil && h.Tailnet.HTTPSState == api.TailnetHTTPSUp {
			if h.Tailnet.HTTPSAddress != secureAddress {
				t.Fatalf("HTTPS address: %+v", h.Tailnet)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTPS never came up: %+v", h.Tailnet)
		}
		time.Sleep(20 * time.Millisecond)
	}
	node := *joined
	spki := sha256.Sum256(node.HTTPSCertificate().RawSubjectPublicKeyInfo)
	options := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(binary), chromedp.NoSandbox,
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("no-proxy-server", true),
		// Keep Chromium's automatic HTTPS upgrade from bypassing the link.
		chromedp.Flag("disable-features", "HttpsUpgrades,HttpsFirstBalancedModeAutoEnable"),
		chromedp.Flag("host-resolver-rules", "MAP "+nodeName+":80 "+s.TailnetAddr()+", MAP "+nodeName+":443 "+node.HTTPSAddr()),
		chromedp.Flag("ignore-certificate-errors-spki-list", base64.StdEncoding.EncodeToString(spki[:])),
		chromedp.Flag("blink-settings", "primaryPointerType=4,availablePointerTypes=4"))
	allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(), options...)
	ctx, cancel := chromedp.NewContext(allocator)
	t.Cleanup(func() { cancel(); cancelAllocator() })
	must(t, chromedp.Run(ctx))
	p := &Page{T: t, Ctx: ctx}
	messageURL := secureAddress + "v1/conversation/" + string(stream)
	responses := make(chan *network.Response, 4)
	chromedp.ListenTarget(ctx, func(event any) {
		if e, ok := event.(*network.EventResponseReceived); ok && e.Response.URL == messageURL {
			select {
			case responses <- e.Response:
			default:
			}
		}
	})
	p.Run(network.Enable(), chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(`
		 window.recognizers = [];
		 window.SpeechRecognition = class {
		  constructor() { recognizers.push(this); this.stops = 0; }
		  start() { this.started = true; }
		  stop() { this.stops++; }
		  result(text) { this.onresult({results: [Object.assign([{transcript: text}], {isFinal:true})]}); }
		 };
		`).Do(ctx)
		return err
	}))
	pagePath := "?source=dictation#workstream=" + string(stream)
	p.Run(chromedp.EmulateViewport(1440, 900), chromedp.Navigate("http://"+nodeName+"/"+pagePath))
	card := `[data-workstream="` + string(stream) + `"]`
	form := card + ` form[data-field=send]`
	mic, link := form+` [aria-label=Dictate]`, form+` a.dictation-https`
	p.Await("the selected conversation over insecure tailnet HTTP", `!isSecureContext && location.href === `+quote("http://"+nodeName+"/"+pagePath)+` && document.querySelector('[data-view=workstream]')?.hidden === false && document.querySelector(`+quote(form)+`) !== null`)
	p.Await("the HTTPS replacement link and hidden microphone", `(() => {
	 const link = document.querySelector(`+quote(link)+`), mic = document.querySelector(`+quote(mic)+`);
	 return link && !link.hidden && link.href === `+quote(secureAddress+pagePath)+` && mic.hidden && getComputedStyle(mic).display === 'none';
	})()`)
	p.AwaitText("#tailnet-https-status", secureAddress)
	// This click leaves the document, including the usual click probe.
	p.Run(chromedp.Click(link, chromedp.ByQuery))
	p.Await("the same conversation on the node's TLS listener", `location.href === `+quote(secureAddress+pagePath)+` && isSecureContext && document.querySelector('[data-view=workstream]')?.hidden === false && document.querySelector(`+quote(form)+`) !== null`)
	p.Await("the visible secure microphone", `(() => {
	 const mic = document.querySelector(`+quote(mic)+`), link = document.querySelector(`+quote(link)+`);
	 return mic && !mic.hidden && mic.getBoundingClientRect().width > 0 && link.hidden;
	})()`)
	const text = "Please keep the upload API stable."
	p.Click(mic)
	p.Await("listening", `recognizers.at(-1)?.started && document.querySelector(`+quote(mic)+`).getAttribute('aria-pressed') === 'true'`)
	p.Eval(`recognizers.at(-1).result(`+quote(text)+`)`, nil)
	p.Await("the dictated draft", `document.querySelector(`+quote(form+" textarea")+`).value === `+quote(text))
	before, err := c.Conversation(context.Background(), stream)
	must(t, err)
	if len(before.Entries) != 0 {
		t.Fatalf("dictation sent before Send: %+v", before.Entries)
	}
	p.Click(form + ` [aria-label=Send]`)
	p.AwaitText(form+" .result", "Sent; the chief of staff answers in its next turn")
	p.Await("stopped recognition and cleared draft", `recognizers.at(-1).stops === 1 && document.querySelector(`+quote(form+" textarea")+`).value === ''`)
	select {
	case response := <-responses:
		_, port, err := net.SplitHostPort(node.HTTPSAddr())
		must(t, err)
		wantPort, err := strconv.Atoi(port)
		must(t, err)
		if response.Status != 200 || response.RemoteIPAddress != "127.0.0.1" || response.RemotePort != int64(wantPort) || response.SecurityDetails == nil {
			t.Fatalf("message did not arrive through the node's TLS listener: %+v", response)
		}
	case <-time.After(BrowserTimeout):
		t.Fatal("no HTTPS message response")
	}
	after, err := c.Conversation(context.Background(), stream)
	must(t, err)
	if len(after.Entries) != 1 || after.Entries[0].Kind != "message" || after.Entries[0].Text != text {
		t.Fatalf("ordinary durable conversation message: %+v", after.Entries)
	}

	// Losing TLS leaves the same node's HTTP page usable, with the real
	// failure reason and neither dictation control.
	node.DropHTTPS()
	var reason string
	deadline = time.Now().Add(BrowserTimeout)
	for {
		h, err := c.Health(context.Background())
		must(t, err)
		if h.Tailnet.HTTPSState == api.TailnetHTTPSUnavailable {
			reason = h.Tailnet.HTTPSReason
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("HTTPS failure never reached status")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reason == "" {
		t.Fatal("HTTPS failure has no reason")
	}
	p.Run(chromedp.Navigate("http://" + nodeName + "/" + pagePath))
	p.AwaitText("#tailnet-https-status", reason)
	p.Await("no dictation controls after TLS failure", `!isSecureContext && document.querySelector(`+quote(mic)+`)?.hidden && document.querySelector(`+quote(link)+`)?.hidden`)
}
