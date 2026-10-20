package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

func TestBrowserMarkdownLiteralFallbackAndInertHTML(t *testing.T) {
	api := newFakeAPI(1)
	hostile := `<script>window.agentScriptRan = true</script>` + "\n" +
		`<img src=x onerror="window.agentImageRan = true">` + "\n" + `<b>literal bold tag</b>` + "\n" +
		`[unsafe](javascript:alert(1)) [data](data:text/html,hello) [relative](/v1/status)`
	cases := []string{
		"Plain first line\nPlain second line\n  indented line with spaces",
		"First paragraph\n\nSecond paragraph\n\n\nThird paragraph\n",
		"Unbalanced **bold marker and *italic marker",
		"Unbalanced single * and double ** markers",
		"Unterminated `inline code",
		"Unterminated [link](https://example.invalid/path",
		"[download](https://example.invalid/_draft_",
		"[download](https://example.invalid/**draft**",
		"[download](https://example.invalid/`draft`",
		"Before the link\n[download](https://example.invalid/_draft_/`code`\nAfter the link",
		"[download](https://example.invalid/(_draft_)",
		"```go\nunterminated <b> code *with markers*",
		"| One | Two |\n| --- | --- |\n| mismatched | two | three |",
		"Unsupported ![image](https://example.invalid/image.png) and ~~strike~~",
		hostile,
	}
	entries := make([]map[string]any, 0, len(cases)+1)
	for i, text := range cases {
		entries = append(entries, map[string]any{"kind": "response", "text": text, "turn": fmt.Sprint(i), "state": "done", "at": "2026-01-01T00:00:00Z"})
	}
	entries = append(entries, map[string]any{"kind": "action", "text": "# Action\n\n**Moved** with *care* and `code`.\n\n- A step\n\n> A quote\n\n[link](https://example.invalid)\n\n| A | B |\n| --- | --- |\n| 1 | 2 |", "state": "done", "turn": "action", "at": "2026-01-01T00:00:00Z"})
	payload, err := json.Marshal(map[string]any{"workstream": "w_000", "entries": entries})
	if err != nil {
		t.Fatal(err)
	}
	fallback := api.handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/feed/") {
			writeJSON(w, string(payload))
			return
		}
		fallback.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(390, 844), chromedp.Navigate(server.URL+"/#workstream=w_000"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.await("all literal fallback replies", `document.querySelectorAll('[data-kind=response]').length === `+strconv.Itoa(len(cases)))
	for i, want := range cases {
		selector := fmt.Sprintf(`[data-kind=response][data-turn="%d"] [data-field=text]`, i)
		var text string
		p.eval(`document.querySelector(`+strconv.Quote(selector)+`).textContent`, &text)
		if text != want {
			t.Errorf("case %d lost literal characters: got %q, want %q", i, text, want)
		}
		if strings.Contains(want, "[download](") {
			var literal bool
			p.eval(`(() => {
  const n = document.querySelector(`+strconv.Quote(selector)+`);
  const p = n.querySelector('p');
  return !n.querySelector('a, em, strong, code') && getComputedStyle(p).whiteSpace === 'pre-wrap' && p.innerText === `+strconv.Quote(want)+`;
})()`, &literal)
			if !literal {
				t.Errorf("case %d formatted an incomplete link or lost visible line breaks", i)
			}
		}
	}
	p.await("literal line breaks remain visible", `(() => {
   const n = document.querySelector('[data-kind=response][data-turn="0"] p');
   return getComputedStyle(n).whiteSpace === 'pre-wrap' && n.innerText === `+strconv.Quote(cases[0])+`;
 })()`)
	p.await("HTML and unsafe links are inert", `!document.querySelector('[data-kind=response] script, [data-kind=response] img, [data-kind=response] b, [data-kind=response] a') &&
   !window.agentScriptRan && !window.agentImageRan`)
	p.await("actions share formatted elements", `(() => {
  const n = document.querySelector('[data-kind=action]');
  return n.querySelector('h1').textContent === 'Action' && n.querySelector('strong').textContent === 'Moved' &&
   n.querySelector('em').textContent === 'care' && n.querySelector('code').textContent === 'code' &&
   n.querySelector('ul li').textContent === 'A step' && n.querySelector('blockquote').textContent === 'A quote' &&
   n.querySelector('a').href === 'https://example.invalid/' && n.querySelectorAll('th').length === 2 && n.querySelectorAll('td').length === 2;
 })()`)
	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if csp := response.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("unexpected CSP: %s", csp)
	}
}
