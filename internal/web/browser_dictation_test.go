package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/chromedp/cdproto/emulation"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// Callbacks stay callable after stop, including a synchronous stop callback,
// so the tests exercise the session fence rather than a cooperative browser.
const fakeDictationScript = `
 window.recognizers = [];
 window.SpeechRecognition = class {
  constructor() { this.starts = 0; this.stops = 0; recognizers.push(this); }
  start() { this.starts++; if (window.failDictationStart) throw new Error('unsupported recognition'); }
  stop() {
   this.stops++;
   this.result('ignored during stop'); this.error('network'); this.end();
  }
  result(text, final = false, previous = []) {
   const result = Object.assign([{transcript:text}], {isFinal:final});
   const results = [...previous.map(t => Object.assign([{transcript:t}], {isFinal:true})), result];
   this.onresult?.({resultIndex: previous.length, results});
  }
  error(error) { this.onerror?.({error}); }
  end() { this.onend?.(); }
 };
 document.addEventListener('DOMContentLoaded', () => document.documentElement.lang = 'fr-CA');
 `

// The regular conversation fixture plus a second design and exploration,
// without agents or microphone services. Sends use the ordinary fake API.
func openDictationPage(t *testing.T, phone bool, script string) (*page, *chatAPI) {
	t.Helper()
	a := newChatAPI()
	handler := a.handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			recorded := httptest.NewRecorder()
			handler.ServeHTTP(recorded, r)
			var status map[string]any
			if err := json.Unmarshal(recorded.Body.Bytes(), &status); err != nil {
				panic(err)
			}
			streams := status["workstreams"].([]any)
			for i, index := range []int{1, 2} {
				clone := make(map[string]any)
				for k, v := range streams[index].(map[string]any) {
					clone[k] = v
				}
				clone["workstream"] = fmt.Sprintf("w_00%d", i+5)
				streams = append(streams, clone)
			}
			status["workstreams"] = streams
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(status)
			return
		}
		switch r.URL.Path {
		case "/v1/sessions/w_005/changes":
			writeJSON(w, `{"changes":"","commits":[]}`)
		case "/v1/design/w_006/conversation":
			writeJSON(w, `{"entries":[]}`)
		default:
			handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	var options []chromedp.ExecAllocatorOption
	if !phone {
		options = append(options, chromedp.Flag("blink-settings", "primaryPointerType=4,availablePointerTypes=4"))
	}
	p := openBrowser(t, options...)
	p.run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
		return err
	}))
	if phone {
		p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), emulation.SetTouchEmulationEnabled(true))
	} else {
		p.run(emulation.SetDeviceMetricsOverride(1440, 900, 1, false))
	}
	p.run(chromedp.Navigate(server.URL + "/"))
	p.await("all conversations", `document.querySelectorAll('#workstream-list [data-select]').length === 7`)
	if phone {
		p.await("a touch coarse pointer", `matchMedia('(pointer: coarse)').matches && !matchMedia('(pointer: fine)').matches && navigator.maxTouchPoints > 0`)
	} else {
		p.await("a desktop fine pointer", `matchMedia('(pointer: fine)').matches && !matchMedia('(pointer: coarse)').matches`)
	}
	return p, a
}

func assertDictation(p *page, expression string) {
	p.t.Helper()
	var ok bool
	p.eval(`!!(`+expression+`)`, &ok)
	if !ok {
		p.t.Fatalf("dictation assertion failed: %s", expression)
	}
}

func checkDictationIcon(p *page, form string, listening bool) {
	p.t.Helper()
	assertDictation(p, `(() => {
  const f = document.querySelector(`+quote(form)+`), b = f.querySelector('[aria-label=Dictate]');
  const svg = b.querySelector('svg'), rect = svg?.querySelector('rect'), path = svg?.querySelector('path');
  const send = f.querySelector('[aria-label=Send]');
  const accent = document.createElement('span'); accent.style.color = 'var(--accent)'; f.append(accent);
  const accentColor = getComputedStyle(accent).color; accent.remove();
  return b.matches('button.icon:not(.primary)') && b.type === 'button' && !b.hidden &&
   b.innerText.trim() === '' && b.parentElement.matches('.actions') && b.nextElementSibling === send &&
   b.getAttribute('aria-pressed') === '`+fmt.Sprint(listening)+`' &&
   svg?.getAttribute('viewBox') === '0 0 24 24' && svg.getAttribute('aria-hidden') === 'true' &&
   rect?.getAttribute('x') === '9' && rect.getAttribute('y') === '3' && rect.getAttribute('width') === '6' &&
   rect.getAttribute('height') === '11' && rect.getAttribute('rx') === '3' &&
   path?.getAttribute('d') === 'M5 11a7 7 0 0 0 14 0M12 18v3' &&
   svg.children.length === 2 && getComputedStyle(svg).strokeWidth === '1.8px' &&
   (getComputedStyle(b).color === accentColor) === `+fmt.Sprint(listening)+` &&
   b.getBoundingClientRect().width > 0 && b.getBoundingClientRect().right <= send.getBoundingClientRect().left;
 })()`)
}

func TestBrowserDictationComposers(t *testing.T) {
	t.Parallel()
	for _, phone := range []bool{false, true} {
		t.Run(fmt.Sprintf("phone=%t", phone), func(t *testing.T) {
			t.Parallel()
			p, a := openDictationPage(t, phone, fakeDictationScript)
			count := 0
			for _, view := range chatViews {
				p.selectChat(view, phone)
				form := view.form
				mic, textarea, result := form+` [aria-label=Dictate]`, form+` textarea`, form+` .result`
				draft := `document.querySelector(` + quote(textarea) + `).value`
				pressed := `document.querySelector(` + quote(mic) + `).getAttribute('aria-pressed')`
				checkDictationIcon(p, form, false)
				p.eval(`(() => { const t = document.querySelector(`+quote(textarea)+`); t.value = 'Before SELECT after'; t.focus(); t.setSelectionRange(7,13); })()`, nil)
				p.click(mic)
				checkDictationIcon(p, form, true)
				assertDictation(p, `recognizers.at(-1).starts === 1 && recognizers.at(-1).interimResults === true && recognizers.at(-1).lang === 'fr-CA'`)
				p.eval(`recognizers.at(-1).result('bonjour')`, nil)
				assertDictation(p, draft+` === 'Before bonjour after'`)
				p.eval(`recognizers.at(-1).result('bonjour monde')`, nil)
				assertDictation(p, draft+` === 'Before bonjour monde after'`)
				p.eval(`recognizers.at(-1).result('Bonjour.',true)`, nil)
				assertDictation(p, draft+` === 'Before Bonjour. after'`)
				p.eval(`recognizers.at(-1).result(' Encore',false,['Bonjour.'])`, nil)
				assertDictation(p, draft+` === 'Before Bonjour. Encore after'`)
				p.eval(`recognizers.at(-1).result(' encore.',true,['Bonjour.'])`, nil)
				assertDictation(p, draft+` === 'Before Bonjour. encore. after'`)
				if got := a.count(); got != count {
					t.Fatalf("dictation sent a message: %d, want %d", got, count)
				}

				p.click(mic)
				checkDictationIcon(p, form, false)
				assertDictation(p, `recognizers.at(-1).stops === 1 && `+draft+` === 'Before Bonjour. encore. after'`)
				// A previous recognizer cannot disturb the next one, even through end.
				p.eval(`window.oldRecognition = recognizers.at(-1); window.previousOutcome = document.querySelector(`+quote(result)+`).textContent`, nil)
				p.click(mic)
				p.eval(`oldRecognition.result('stale'); oldRecognition.error('not-allowed'); oldRecognition.end()`, nil)
				checkDictationIcon(p, form, true)
				assertDictation(p, draft+` === 'Before Bonjour. encore. after' && document.querySelector(`+quote(result)+`).textContent === previousOutcome`)
				p.eval(`recognizers.at(-1).result(' Ajout.',true)`, nil)
				assertDictation(p, draft+` === 'Before Bonjour. encore. Ajout. after'`)
				// Sending must invalidate first: stop itself emits hostile callbacks.
				release := sync.OnceFunc(a.action.holdResponse())
				t.Cleanup(release)
				p.click(form + ` [aria-label=Send]`)
				count++
				assertChatMessage(t, a, count, view, "dictated click", "Before Bonjour. encore. Ajout. after")
				assertDictation(p, `recognizers.at(-1).stops === 1 && `+pressed+` === 'false'`)
				p.awaitText(result, "Sending…")
				p.eval(`recognizers.at(-1).result('late pending'); recognizers.at(-1).error('network'); recognizers.at(-1).end()`, nil)
				assertDictation(p, `document.querySelector(`+quote(result)+`).textContent === 'Sending…'`)
				release()
				p.awaitText(result, view.result)
				p.await("cleared input after send", draft+` === ''`)
				p.eval(`recognizers.at(-1).result('late after send'); recognizers.at(-1).error('not-allowed'); recognizers.at(-1).end()`, nil)
				assertDictation(p, draft+` === '' && document.querySelector(`+quote(result)+`).textContent === `+quote(view.result))

				for _, failure := range []struct{ code, text string }{{"not-allowed", "permission denied"}, {"no-speech", "No speech"}, {"network", "network error"}, {"language-not-supported", "unavailable"}} {
					p.eval(`document.querySelector(`+quote(textarea)+`).value = 'Keep this draft'`, nil)
					p.click(mic)
					p.eval(`recognizers.at(-1).error(`+quote(failure.code)+`)`, nil)
					p.awaitText(result, failure.text)
					assertDictation(p, draft+` === 'Keep this draft' && `+pressed+` === 'false'`)
				}
				p.eval(`window.failDictationStart = true`, nil)
				p.click(mic)
				p.awaitText(result, "Could not start dictation")
				assertDictation(p, draft+` === 'Keep this draft' && `+pressed+` === 'false'`)
				p.eval(`window.failDictationStart = false`, nil)
				// Natural completion also fences anything the recognizer reports later.
				p.click(mic)
				p.eval(`recognizers.at(-1).end(); recognizers.at(-1).result('after end')`, nil)
				assertDictation(p, draft+` === 'Keep this draft' && `+pressed+` === 'false'`)

				p.click(mic)
				settleEnter := sync.OnceFunc(a.action.holdResponse())
				t.Cleanup(settleEnter)
				if phone {
					p.run(chromedp.Focus(textarea, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
					assertDictation(p, pressed+` === 'true'`)
					if got := a.count(); got != count {
						t.Fatalf("phone Enter sent: %d, want %d", got, count)
					}
					p.click(form + ` [aria-label=Send]`)
				} else {
					p.run(chromedp.Focus(textarea, chromedp.ByQuery))
					p.await("focused desktop textarea", `document.activeElement === document.querySelector(`+quote(textarea)+`) && matchMedia('(pointer: fine)').matches`)
					p.run(chromedp.KeyEvent(kb.Enter))
				}
				count++
				assertChatMessage(t, a, count, view, "dictated Enter", "Keep this draft")
				p.awaitText(result, "Sending…")
				settleEnter()
				p.awaitText(result, view.result)
				p.await("settled Enter response", draft+` === '' && `+pressed+` === 'false'`)
				assertDictation(p, `recognizers.at(-1).stops === 1`)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if len(a.posts) != count {
				t.Fatalf("extra requests (possibly audio): %v", a.posts)
			}
		})
	}
}

func TestBrowserDictationAvailability(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"absent", "insecure", "webkit"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			script := `delete window.SpeechRecognition; delete window.webkitSpeechRecognition;`
			switch mode {
			case "insecure":
				script = fakeDictationScript + `Object.defineProperty(window,'isSecureContext',{value:false});`
			case "webkit":
				script = fakeDictationScript + `window.webkitSpeechRecognition = window.SpeechRecognition; delete window.SpeechRecognition;`
			}
			p, _ := openDictationPage(t, false, script)
			for _, view := range chatViews {
				p.selectChat(view, false)
				mic := view.form + ` [aria-label=Dictate]`
				if mode == "webkit" {
					checkDictationIcon(p, view.form, false)
					p.click(mic)
					assertDictation(p, `recognizers.at(-1).starts === 1`)
					p.click(mic)
				} else {
					assertDictation(p, `document.querySelector(`+quote(mic)+`).hidden && getComputedStyle(document.querySelector(`+quote(mic)+`)).display === 'none'`)
				}
			}
		})
	}
}

func TestBrowserDictationConversationOwnership(t *testing.T) {
	t.Parallel()
	for _, phone := range []bool{false, true} {
		t.Run(fmt.Sprintf("phone=%t", phone), func(t *testing.T) {
			t.Parallel()
			p, _ := openDictationPage(t, phone, fakeDictationScript)
			for _, view := range chatViews {
				p.selectChat(view, phone)
				form := view.form
				mic, textarea, result := form+` [aria-label=Dictate]`, form+` textarea`, form+` .result`
				p.typeInto(textarea, "A draft")
				p.click(mic)
				p.eval(`window.aRecognition = recognizers.at(-1)`, nil)
				other := chatViews[0]
				switch view.name {
				case "workstream":
					other = chatViews[1]
				case "session":
					other = chatView{name: "session", id: "w_005", form: "#session-form"}
				case "design":
					other = chatView{name: "design", id: "w_006", form: "#design-form"}
				}
				p.selectChat(other, phone)
				assertDictation(p, `aRecognition.stops === 1 && document.querySelector(`+quote(mic)+`).getAttribute('aria-pressed') === 'false'`)
				bMic, bText, bResult := other.form+` [aria-label=Dictate]`, other.form+` textarea`, other.form+` .result`
				p.eval(`document.querySelector(`+quote(bText)+`).value = 'B draft'; window.bOutcome = document.querySelector(`+quote(bResult)+`).textContent`, nil)
				p.eval(`aRecognition.result('late A'); aRecognition.error('network'); aRecognition.end()`, nil)
				assertDictation(p, `document.querySelector(`+quote(bText)+`).value === 'B draft' && document.querySelector(`+quote(bResult)+`).textContent === bOutcome && document.querySelector(`+quote(bMic)+`).getAttribute('aria-pressed') === 'false'`)
				p.click(bMic)
				p.eval(`aRecognition.result('late A again'); aRecognition.error('not-allowed'); aRecognition.end()`, nil)
				assertDictation(p, `document.querySelector(`+quote(bText)+`).value === 'B draft' && document.querySelector(`+quote(bResult)+`).textContent === bOutcome && document.querySelector(`+quote(bMic)+`).getAttribute('aria-pressed') === 'true'`)
				p.click(bMic)
				// Returning to A cannot reactivate its ended recognition session.
				p.selectChat(view, phone)
				p.eval(`window.returnDraft = document.querySelector(`+quote(textarea)+`).value; window.returnOutcome = document.querySelector(`+quote(result)+`).textContent`, nil)
				p.click(mic)
				p.eval(`aRecognition.result('old A on return'); aRecognition.error('no-speech'); aRecognition.end()`, nil)
				assertDictation(p, `document.querySelector(`+quote(textarea)+`).value === returnDraft && document.querySelector(`+quote(result)+`).textContent === returnOutcome && document.querySelector(`+quote(mic)+`).getAttribute('aria-pressed') === 'true'`)
				p.click(mic)
			}
		})
	}
}
