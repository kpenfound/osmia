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

// The page loads its bundled sans and mono from its own origin without a
// content-security-policy violation, and the face a text node uses is the
// bundled one rather than a system font.
func TestBrowserPageLoadsItsBundledFontsWithoutPolicyViolations(t *testing.T) {
	t.Parallel()
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

	var violations []string
	p.eval(`window.__violations`, &violations)
	if len(violations) != 0 {
		t.Fatalf("content-security-policy violations: %q", violations)
	}

	var served []string
	p.eval(`performance.getEntriesByType('resource').filter((r) => r.name.endsWith('.woff2')).map((r) => new URL(r.name).pathname + ' ' + (r.decodedBodySize > 0))`, &served)
	if len(served) != 2 {
		t.Fatalf("font requests: %q", served)
	}

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
