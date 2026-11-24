package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func openFontsPage(t *testing.T) *page {
	t.Helper()
	api := newFakeAPI(1)
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := cdppage.AddScriptToEvaluateOnNewDocument(`window.__violations = [];
document.addEventListener('securitypolicyviolation', (e) => window.__violations.push(e.violatedDirective + ' ' + e.blockedURI));`).Do(ctx)
			return err
		}),
		chromedp.Navigate(server.URL+"/"))
	p.await("the workstream list", `document.querySelectorAll('#workstream-list [data-select]').length === 1`)
	return p
}

func TestBrowserPageLoadsItsBundledFontFaces(t *testing.T) {
	t.Parallel()
	p := openFontsPage(t)
	var loaded []string
	p.run(chromedp.Evaluate(`(async () => {
		await Promise.all([document.fonts.load('400 14px "Instrument Sans"'), document.fonts.load('600 14px "Instrument Sans"'), document.fonts.load('400 14px "JetBrains Mono"')]);
		await document.fonts.ready;
		return [...document.fonts].filter((f) => f.status === 'loaded').map((f) => f.family.replaceAll('"', ''));
	})()`, &loaded, func(params *runtime.EvaluateParams) *runtime.EvaluateParams { return params.WithAwaitPromise(true) }))
	joined := strings.Join(loaded, ",")
	if !strings.Contains(joined, "Instrument Sans") || !strings.Contains(joined, "JetBrains Mono") {
		t.Fatalf("loaded font faces: %q", loaded)
	}
}

func TestBrowserPageLoadsItsFontsWithoutPolicyViolations(t *testing.T) {
	t.Parallel()
	p := openFontsPage(t)
	p.run(chromedp.Evaluate(`document.fonts.ready.then(() => true)`, new(bool), func(params *runtime.EvaluateParams) *runtime.EvaluateParams { return params.WithAwaitPromise(true) }))
	var violations []string
	p.eval(`window.__violations`, &violations)
	if len(violations) != 0 {
		t.Fatalf("content-security-policy violations: %q", violations)
	}
}

func TestBrowserPageRequestsEachBundledFontFileOnce(t *testing.T) {
	t.Parallel()
	p := openFontsPage(t)
	p.run(chromedp.Evaluate(`document.fonts.ready.then(() => true)`, new(bool), func(params *runtime.EvaluateParams) *runtime.EvaluateParams { return params.WithAwaitPromise(true) }))
	var served []string
	p.eval(`performance.getEntriesByType('resource').filter((r) => r.name.endsWith('.woff2')).map((r) => new URL(r.name).pathname + ' ' + (r.decodedBodySize > 0))`, &served)
	if len(served) != 2 {
		t.Fatalf("font requests: %q", served)
	}
}

func TestBrowserPageSetsBodyTextInInstrumentSansAndCodeInJetBrainsMono(t *testing.T) {
	t.Parallel()
	p := openFontsPage(t)
	var faces struct{ Body, Code string }
	p.eval(`(() => {
		const code = document.createElement('code'); code.textContent = 'w_01'; document.body.append(code);
		const out = { Body: getComputedStyle(document.body).fontFamily, Code: getComputedStyle(code).fontFamily };
		code.remove();
		return out;
	})()`, &faces)
	if !strings.HasPrefix(faces.Body, `"Instrument Sans"`) || !strings.HasPrefix(faces.Code, `"JetBrains Mono"`) {
		t.Fatalf("font families: %+v", faces)
	}
}
