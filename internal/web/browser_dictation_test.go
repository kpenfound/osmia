package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// fakeDictationScript replaces the browser's microphone and recorder. A
// recorder reports its clip and its end a moment after stop, as a browser
// does, and the test can call data and finish again on a recorder that has
// long ended, so the tests exercise the recording fence rather than a
// cooperative browser. Setting mediaError makes the microphone refuse;
// setting silence makes recorders produce an empty clip. Every settled
// transcription fetch is counted in transcribeSettled once the page has had
// a moment to act on the answer.
const fakeDictationScript = `
 window.recorders = [];
 window.tracks = [];
 window.transcribeSettled = 0;
 Object.defineProperty(navigator, 'mediaDevices', {configurable: true, value: {
  getUserMedia: async constraints => {
   window.mediaConstraints = constraints;
   if (window.mediaError) throw Object.assign(new Error('refused'), {name: window.mediaError});
   const track = {stopped: false, stop() { this.stopped = true; }};
   tracks.push(track);
   return {getTracks: () => [track]};
  },
 }});
 window.MediaRecorder = class {
  static isTypeSupported(type) { return type === 'audio/webm;codecs=opus'; }
  constructor(stream, options = {}) {
   this.stream = stream; this.mimeType = options.mimeType || ''; this.state = 'inactive';
   this.starts = 0; this.stops = 0; this.delivered = false;
   recorders.push(this);
  }
  start() { this.starts++; this.state = 'recording'; }
  stop() {
   if (this.state === 'inactive') return;
   this.state = 'inactive'; this.stops++;
   setTimeout(() => { this.data(window.silence ? '' : 'voice'); this.finish(); this.delivered = true; }, 0);
  }
  data(text) { this.ondataavailable?.({data: new Blob(text ? [text] : [], {type: this.mimeType})}); }
  error(message) { this.onerror?.({error: new Error(message)}); }
  finish() { this.onstop?.(); }
 };
 const realFetch = window.fetch;
 window.fetch = async (...args) => {
  try { return await realFetch(...args); }
  finally { if (String(args[0]).includes('/transcribe')) setTimeout(() => window.transcribeSettled++, 100); }
 };
 `

// transcribeRequest is one upload the fake transcription route received.
type transcribeRequest struct {
	ContentType string
	MIMEType    string
	Audio       []byte
}

// fakeTranscribe stands in for the service's transcription route and for
// the configuration that says whether transcription is on. It answers the
// queued texts in order, can be held open, and can be made to refuse.
type fakeTranscribe struct {
	mu       sync.Mutex
	url      string
	texts    []string
	status   int
	code     string
	message  string
	hold     chan struct{}
	requests []transcribeRequest
	answered int
}

func (f *fakeTranscribe) setURL(url string) {
	f.mu.Lock()
	f.url = url
	f.mu.Unlock()
}

// answer queues the texts the next uploads are answered with.
func (f *fakeTranscribe) answer(texts ...string) {
	f.mu.Lock()
	f.texts = append(f.texts, texts...)
	f.mu.Unlock()
}

// refuse makes every upload from now on fail with the status, code and
// message, until refuse(0, "", "") restores answers.
func (f *fakeTranscribe) refuse(status int, code, message string) {
	f.mu.Lock()
	f.status, f.code, f.message = status, code, message
	f.mu.Unlock()
}

// holdResponse makes every upload answer only once release is called.
func (f *fakeTranscribe) holdResponse() (release func()) {
	ch := make(chan struct{})
	f.mu.Lock()
	f.hold = ch
	f.mu.Unlock()
	return func() { close(ch) }
}

func (f *fakeTranscribe) handleConfig(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	url := f.url
	f.mu.Unlock()
	writeJSON(w, fmt.Sprintf(`{"root":"","digest":"fake","effective":{"profiles":{},"transcribe":{"url":%s,"model":"large-v3-turbo","api_key_env":"WHISPER_API_KEY"}},`+
		`"project":null,"projects":[],"diagnostics":[],"drift":{"differs":false,"files":[]}}`, quote(url)))
}

func (f *fakeTranscribe) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MIMEType string `json:"mime_type"`
		Audio    string `json:"audio"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "expected a JSON POST", http.StatusBadRequest)
		return
	}
	audio, err := base64.StdEncoding.DecodeString(body.Audio)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, transcribeRequest{r.Header.Get("Content-Type"), body.MIMEType, audio})
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	defer func() { f.answered++ }()
	if f.status != 0 {
		w.WriteHeader(f.status)
		writeJSON(w, fmt.Sprintf(`{"error":{"code":%s,"message":%s}}`, quote(f.code), quote(f.message)))
		return
	}
	text := ""
	if len(f.texts) > 0 {
		text, f.texts = f.texts[0], f.texts[1:]
	}
	writeJSON(w, fmt.Sprintf(`{"text":%s}`, quote(text)))
}

// uploads returns how many clips the route received.
func (f *fakeTranscribe) uploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// awaitUploads waits until the route has received n clips in total.
func (f *fakeTranscribe) awaitUploads(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(browserTimeout)
	for f.uploads() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the transcription route received %d uploads, want %d", f.uploads(), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fakeTranscribe) upload(i int) transcribeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

// openDictationPage serves the regular conversation fixture plus a second
// design and exploration, without agents or microphone services. Sends use
// the ordinary fake API. Each option adjusts the fake before the page loads.
// The page's requests to any other origin are recorded in the returned list.
func openDictationPage(t *testing.T, phone bool, script string, options ...func(*chatAPI)) (*page, *chatAPI, func() []string) {
	t.Helper()
	a := newChatAPI()
	for _, option := range options {
		option(a)
	}
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
	var browserOptions []chromedp.ExecAllocatorOption
	if !phone {
		browserOptions = append(browserOptions, chromedp.Flag("blink-settings", "primaryPointerType=4,availablePointerTypes=4"))
	}
	p := openBrowser(t, browserOptions...)
	var mu sync.Mutex
	var foreign []string
	chromedp.ListenTarget(p.ctx, func(event any) {
		if e, ok := event.(*network.EventRequestWillBeSent); ok &&
			(strings.HasPrefix(e.Request.URL, "http:") || strings.HasPrefix(e.Request.URL, "https:") ||
				strings.HasPrefix(e.Request.URL, "ws:") || strings.HasPrefix(e.Request.URL, "wss:")) &&
			!strings.HasPrefix(e.Request.URL, server.URL+"/") {
			mu.Lock()
			foreign = append(foreign, e.Request.URL)
			mu.Unlock()
		}
	})
	p.run(network.Enable(), chromedp.ActionFunc(func(ctx context.Context) error {
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
	return p, a, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), foreign...)
	}
}

func assertDictation(p *page, expression string) {
	p.t.Helper()
	var ok bool
	p.eval(`!!(`+expression+`)`, &ok)
	if !ok {
		p.t.Fatalf("dictation assertion failed: %s", expression)
	}
}

func checkDictationIcon(p *page, form string, recording bool) {
	p.t.Helper()
	assertDictation(p, `(() => {
  const f = document.querySelector(`+quote(form)+`), b = f.querySelector('[aria-label=Dictate]');
  const svg = b.querySelector('svg'), rect = svg?.querySelector('rect'), path = svg?.querySelector('path');
  const send = f.querySelector('[aria-label=Send]');
  const accent = document.createElement('span'); accent.style.color = 'var(--accent)'; f.append(accent);
  const accentColor = getComputedStyle(accent).color; accent.remove();
  return b.matches('button.icon:not(.primary)') && b.type === 'button' && !b.hidden &&
   b.innerText.trim() === '' && b.parentElement.matches('.actions') && b.nextElementSibling === send &&
   b.getAttribute('aria-pressed') === '`+fmt.Sprint(recording)+`' &&
   svg?.getAttribute('viewBox') === '0 0 24 24' && svg.getAttribute('aria-hidden') === 'true' &&
   rect?.getAttribute('x') === '9' && rect.getAttribute('y') === '3' && rect.getAttribute('width') === '6' &&
   rect.getAttribute('height') === '11' && rect.getAttribute('rx') === '3' &&
   path?.getAttribute('d') === 'M5 11a7 7 0 0 0 14 0M12 18v3' &&
   svg.children.length === 2 && getComputedStyle(svg).strokeWidth === '1.8px' &&
   (getComputedStyle(b).color === accentColor) === `+fmt.Sprint(recording)+` &&
   b.getBoundingClientRect().width > 0 && b.getBoundingClientRect().right <= send.getBoundingClientRect().left;
 })()`)
}

// startRecording taps the microphone and waits until the recorder runs.
func startRecording(p *page, form string) {
	p.t.Helper()
	var before int
	p.eval(`recorders.length`, &before)
	p.click(form + ` [aria-label=Dictate]`)
	p.await("a running recorder", fmt.Sprintf(`recorders.length === %d && recorders.at(-1).state === 'recording'`, before+1))
	checkDictationIcon(p, form, true)
}

// stopRecording taps the microphone again and waits for the upload to start.
func stopRecording(p *page, form string) {
	p.t.Helper()
	p.click(form + ` [aria-label=Dictate]`)
	p.awaitText(form+` .result`, "Transcribing")
	checkDictationIcon(p, form, false)
}

// awaitAnswerSeen waits until the page has received the n-th transcription
// answer and had time to act on it.
func awaitAnswerSeen(p *page, n int) {
	p.t.Helper()
	p.await("the transcription answer", fmt.Sprintf(`transcribeSettled >= %d`, n))
}

func TestBrowserDictationComposers(t *testing.T) {
	t.Parallel()
	for _, phone := range []bool{false, true} {
		t.Run(fmt.Sprintf("phone=%t", phone), func(t *testing.T) {
			t.Parallel()
			p, a, foreign := openDictationPage(t, phone, fakeDictationScript)
			count, uploads := 0, 0
			for _, view := range chatViews {
				p.selectChat(view, phone)
				form := view.form
				mic, textarea, result := form+` [aria-label=Dictate]`, form+` textarea`, form+` .result`
				draft := `document.querySelector(` + quote(textarea) + `).value`
				outcome := `document.querySelector(` + quote(result) + `).textContent`
				pressed := `document.querySelector(` + quote(mic) + `).getAttribute('aria-pressed')`
				idle := `document.querySelector(` + quote(mic) + `).getAttribute('aria-pressed') === 'false' && !document.querySelector(` + quote(mic) + `).disabled`
				setDraft := func(text string) {
					p.eval(`(() => { const t = document.querySelector(`+quote(textarea)+`); t.value = `+quote(text)+`; t.focus(); t.setSelectionRange(7, 13); })()`, nil)
				}
				checkDictationIcon(p, form, false)

				// A recording starts on the first tap and uploads on the second; the
				// existing text stays whatever the selection, and the answer follows it.
				setDraft("Before SELECT after")
				a.transcribe.answer("hello world")
				startRecording(p, form)
				assertDictation(p, `recorders.at(-1).starts === 1 && recorders.at(-1).mimeType === 'audio/webm;codecs=opus' && mediaConstraints.audio === true`)
				release := sync.OnceFunc(a.transcribe.holdResponse())
				t.Cleanup(release)
				stopRecording(p, form)
				uploads++
				a.transcribe.awaitUploads(t, uploads)
				assertDictation(p, `recorders.at(-1).stops === 1 && tracks.at(-1).stopped`)
				if got := a.transcribe.upload(uploads - 1); got.ContentType != "application/json" || got.MIMEType != "audio/webm;codecs=opus" || string(got.Audio) != "voice" {
					t.Fatalf("%s upload = %+v", view.name, got)
				}
				assertDictation(p, draft+` === 'Before SELECT after' && `+outcome+`.includes('Transcribing')`)
				release()
				p.await("the appended transcription", draft+` === 'Before SELECT after hello world' && `+outcome+` === ''`)
				assertDictation(p, idle)

				// A second recording appends again; an empty draft receives the text as is.
				a.transcribe.answer("and more")
				startRecording(p, form)
				p.click(mic)
				p.await("the second transcription", draft+` === 'Before SELECT after hello world and more'`)
				assertDictation(p, idle)
				setDraft("")
				a.transcribe.answer("Just the words.")
				startRecording(p, form)
				p.click(mic)
				uploads += 2
				p.await("the transcription of an empty draft", draft+` === 'Just the words.'`)
				if got := a.count(); got != count {
					t.Fatalf("%s: recording sent a message: %d, want %d", view.name, got, count)
				}

				// A refused microphone, an empty recording, an unconfigured service, a
				// failed transcription and a silent one leave the draft alone.
				setDraft("Keep this draft")
				p.eval(`window.mediaError = 'NotAllowedError'`, nil)
				p.click(mic)
				p.awaitText(result, "Microphone permission denied")
				assertDictation(p, draft+` === 'Keep this draft' && `+idle)
				p.eval(`window.mediaError = null; window.silence = true`, nil)
				startRecording(p, form)
				p.click(mic)
				p.awaitText(result, "Nothing was recorded")
				assertDictation(p, draft+` === 'Keep this draft' && `+idle)
				p.eval(`window.silence = false`, nil)
				for _, failure := range []struct {
					status        int
					code, message string
				}{
					{http.StatusConflict, "transcription_off", "transcription is not configured on this service"},
					{http.StatusBadGateway, "transcription_failed", "the transcription server could not be reached"},
				} {
					a.transcribe.refuse(failure.status, failure.code, failure.message)
					startRecording(p, form)
					p.click(mic)
					uploads++
					p.awaitText(result, failure.message)
					assertDictation(p, draft+` === 'Keep this draft' && `+idle+` && `+outcome+`.length > 0`)
				}
				a.transcribe.refuse(0, "", "")
				a.transcribe.answer("   ")
				startRecording(p, form)
				p.click(mic)
				uploads++
				p.awaitText(result, "No speech was detected")
				assertDictation(p, draft+` === 'Keep this draft' && `+idle)
				if got := a.transcribe.uploads(); got != uploads {
					t.Fatalf("%s: %d uploads, want %d", view.name, got, uploads)
				}

				// A recording with a microphone that fails while recording ends it.
				startRecording(p, form)
				p.eval(`recorders.at(-1).error('device lost')`, nil)
				p.awaitText(result, "Recording failed")
				assertDictation(p, draft+` === 'Keep this draft' && `+idle+` && tracks.at(-1).stopped && recorders.at(-1).stops === 1`)

				// Sending stops the recording first: a stopped recorder that still
				// reports its clip uploads nothing and changes neither draft nor outcome.
				startRecording(p, form)
				releaseSend := sync.OnceFunc(a.action.holdResponse())
				t.Cleanup(releaseSend)
				p.click(form + ` [aria-label=Send]`)
				count++
				assertChatMessage(t, a, count, view, "send while recording", "Keep this draft")
				assertDictation(p, `recorders.at(-1).stops === 1 && tracks.at(-1).stopped && `+pressed+` === 'false'`)
				p.awaitText(result, "Sending…")
				p.await("the stopped recorder's callbacks", `recorders.at(-1).delivered`)
				p.eval(`recorders.at(-1).data('late'); recorders.at(-1).finish()`, nil)
				assertDictation(p, `document.querySelector(`+quote(result)+`).textContent === 'Sending…'`)
				releaseSend()
				p.awaitText(result, view.result)
				p.await("cleared input after send", draft+` === ''`)
				if got := a.transcribe.uploads(); got != uploads {
					t.Fatalf("%s: a recording stopped by Send uploaded: %d uploads, want %d", view.name, got, uploads)
				}

				// Sending while the transcription is in progress discards its answer.
				setDraft("Keep this draft")
				a.transcribe.answer("late words")
				releaseTranscription := sync.OnceFunc(a.transcribe.holdResponse())
				t.Cleanup(releaseTranscription)
				startRecording(p, form)
				stopRecording(p, form)
				uploads++
				a.transcribe.awaitUploads(t, uploads)
				answers := a.transcribe.answeredCount()
				releaseSend = sync.OnceFunc(a.action.holdResponse())
				t.Cleanup(releaseSend)
				p.click(form + ` [aria-label=Send]`)
				count++
				assertChatMessage(t, a, count, view, "send while transcribing", "Keep this draft")
				p.awaitText(result, "Sending…")
				releaseTranscription()
				a.transcribe.awaitAnswered(t, answers+1)
				awaitAnswerSeen(p, uploads)
				assertDictation(p, draft+` === 'Keep this draft' && `+outcome+` === 'Sending…'`)
				releaseSend()
				p.awaitText(result, view.result)
				p.await("cleared input after the send", draft+` === '' && `+idle)
				// The late words are not used by the next recording.
				a.transcribe.refuse(0, "", "")
				a.transcribe.answer("fresh words")
				startRecording(p, form)
				p.click(mic)
				uploads++
				p.await("only the next transcription", draft+` === 'fresh words'`)

				// Send and Enter still send the edited draft.
				p.eval(`(() => { const t = document.querySelector(`+quote(textarea)+`); t.value += ' (edited)'; })()`, nil)
				if phone {
					p.run(chromedp.Focus(textarea, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
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
				assertChatMessage(t, a, count, view, "edited transcription", "fresh words (edited)")
				p.awaitText(result, view.result)
				p.await("cleared input after Enter", draft+` === ''`)
			}
			a.mu.Lock()
			var extra []string
			for _, path := range a.posts {
				if path != "/v1/transcribe" {
					extra = append(extra, path)
				}
			}
			a.mu.Unlock()
			if len(extra) != count {
				t.Fatalf("requests other than transcriptions and messages: %v, want %d messages", extra, count)
			}
			if got := foreign(); len(got) != 0 {
				t.Fatalf("the page contacted other origins: %v", got)
			}
		})
	}
}

func (f *fakeTranscribe) answeredCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answered
}

func (f *fakeTranscribe) awaitAnswered(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(browserTimeout)
	for f.answeredCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the transcription route answered %d uploads, want %d", f.answeredCount(), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBrowserDictationAvailability(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"available", "transcription off", "no recorder", "no microphone api", "insecure"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			script := fakeDictationScript
			var options []func(*chatAPI)
			switch mode {
			case "transcription off":
				options = append(options, func(a *chatAPI) { a.transcribe.setURL("") })
			case "no recorder":
				script += `delete window.MediaRecorder;`
			case "no microphone api":
				script += `Object.defineProperty(navigator, 'mediaDevices', {configurable: true, value: undefined});`
			case "insecure":
				script += `Object.defineProperty(window, 'isSecureContext', {value: false});`
			}
			p, _, _ := openDictationPage(t, false, script, options...)
			for _, view := range chatViews {
				p.selectChat(view, false)
				mic := view.form + ` [aria-label=Dictate]`
				if mode == "available" {
					checkDictationIcon(p, view.form, false)
				} else {
					assertDictation(p, `document.querySelector(`+quote(mic)+`).hidden && getComputedStyle(document.querySelector(`+quote(mic)+`)).display === 'none'`)
				}
			}
		})
	}
}

// Turning transcription off or on in the configuration hides or shows the
// microphone without a reload, and ends a recording in progress.
func TestBrowserDictationFollowsTranscriptionConfig(t *testing.T) {
	t.Parallel()
	p, a, _ := openDictationPage(t, false, fakeDictationScript)
	view := chatViews[0]
	p.selectChat(view, false)
	mic := view.form + ` [aria-label=Dictate]`
	checkDictationIcon(p, view.form, false)
	startRecording(p, view.form)
	a.transcribe.setURL("")
	a.base.pushEvent("resync")
	p.await("a hidden microphone", `document.querySelector(`+quote(mic)+`).hidden`)
	assertDictation(p, `recorders.at(-1).stops === 1 && tracks.at(-1).stopped && document.querySelector(`+quote(mic)+`).getAttribute('aria-pressed') === 'false'`)
	a.transcribe.setURL("http://whisper.test/v1/audio/transcriptions")
	a.base.pushEvent("resync")
	p.await("a shown microphone", `!document.querySelector(`+quote(mic)+`).hidden`)
	checkDictationIcon(p, view.form, false)
	if got := a.transcribe.uploads(); got != 0 {
		t.Fatalf("a recording ended by the configuration uploaded: %d", got)
	}
}

func TestBrowserDictationConversationOwnership(t *testing.T) {
	t.Parallel()
	for _, phone := range []bool{false, true} {
		t.Run(fmt.Sprintf("phone=%t", phone), func(t *testing.T) {
			t.Parallel()
			p, a, _ := openDictationPage(t, phone, fakeDictationScript)
			uploads := 0
			for _, view := range chatViews {
				p.selectChat(view, phone)
				form := view.form
				mic, textarea, result := form+` [aria-label=Dictate]`, form+` textarea`, form+` .result`
				other := chatViews[0]
				switch view.name {
				case "workstream":
					other = chatViews[1]
				case "session":
					other = chatView{name: "session", id: "w_005", form: "#session-form"}
				case "design":
					other = chatView{name: "design", id: "w_006", form: "#design-form"}
				}
				bMic, bText, bResult := other.form+` [aria-label=Dictate]`, other.form+` textarea`, other.form+` .result`

				// Switching conversation while recording stops the recorder; what the
				// stopped recorder reports later reaches no composer.
				p.typeInto(textarea, "A draft")
				startRecording(p, form)
				p.eval(`window.aRecorder = recorders.at(-1); window.aTrack = tracks.at(-1)`, nil)
				p.selectChat(other, phone)
				assertDictation(p, `aRecorder.stops === 1 && aTrack.stopped && document.querySelector(`+quote(mic)+`).getAttribute('aria-pressed') === 'false'`)
				p.eval(`document.querySelector(`+quote(bText)+`).value = 'B draft'; window.bOutcome = document.querySelector(`+quote(bResult)+`).textContent`, nil)
				p.await("the stopped recorder's callbacks", `aRecorder.delivered`)
				p.eval(`aRecorder.data('late A'); aRecorder.finish(); aRecorder.error('late')`, nil)
				assertDictation(p, `document.querySelector(`+quote(bText)+`).value === 'B draft' && document.querySelector(`+quote(bResult)+`).textContent === bOutcome && document.querySelector(`+quote(bMic)+`).getAttribute('aria-pressed') === 'false'`)
				if got := a.transcribe.uploads(); got != uploads {
					t.Fatalf("%s: a recording left behind uploaded: %d uploads, want %d", view.name, got, uploads)
				}
				// Returning to A cannot reactivate its ended recording.
				p.selectChat(view, phone)
				p.eval(`window.returnDraft = document.querySelector(`+quote(textarea)+`).value; window.returnOutcome = document.querySelector(`+quote(result)+`).textContent`, nil)
				p.eval(`aRecorder.data('old A on return'); aRecorder.finish()`, nil)
				assertDictation(p, `document.querySelector(`+quote(textarea)+`).value === returnDraft && document.querySelector(`+quote(result)+`).textContent === returnOutcome && document.querySelector(`+quote(mic)+`).getAttribute('aria-pressed') === 'false'`)

				// A transcription finishing after the owner left the conversation
				// changes neither it nor the conversation now shown, and leaves no
				// progress message behind.
				a.transcribe.answer("answer for A")
				release := sync.OnceFunc(a.transcribe.holdResponse())
				t.Cleanup(release)
				startRecording(p, form)
				stopRecording(p, form)
				uploads++
				a.transcribe.awaitUploads(t, uploads)
				answers := a.transcribe.answeredCount()
				p.selectChat(other, phone)
				p.eval(`document.querySelector(`+quote(bText)+`).value = 'B draft'; window.bOutcome = document.querySelector(`+quote(bResult)+`).textContent`, nil)
				release()
				a.transcribe.awaitAnswered(t, answers+1)
				awaitAnswerSeen(p, uploads)
				assertDictation(p, `document.querySelector(`+quote(bText)+`).value === 'B draft' && document.querySelector(`+quote(bResult)+`).textContent === bOutcome && document.querySelector(`+quote(bMic)+`).getAttribute('aria-pressed') === 'false'`)
				p.selectChat(view, phone)
				assertDictation(p, `document.querySelector(`+quote(textarea)+`).value === returnDraft && document.querySelector(`+quote(result)+`).textContent === '' && document.querySelector(`+quote(mic)+`).getAttribute('aria-pressed') === 'false' && !document.querySelector(`+quote(mic)+`).disabled`)

				// The same microphone records again afterwards.
				a.transcribe.answer("second")
				startRecording(p, form)
				p.click(mic)
				uploads++
				p.await("a fresh transcription", `document.querySelector(`+quote(textarea)+`).value === (returnDraft ? returnDraft + ' ' : '') + 'second'`)
			}
		})
	}
}
