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

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

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

type transcribeRequest struct {
	ContentType string
	MIMEType    string
	Audio       []byte
}

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

	uploaded, responded counter
}

func (f *fakeTranscribe) setURL(url string) {
	f.mu.Lock()
	f.url = url
	f.mu.Unlock()
}

func (f *fakeTranscribe) answer(texts ...string) {
	f.mu.Lock()
	f.texts = append(f.texts, texts...)
	f.mu.Unlock()
}

func (f *fakeTranscribe) refuse(status int, code, message string) {
	f.mu.Lock()
	f.status, f.code, f.message = status, code, message
	f.mu.Unlock()
}

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
	f.uploaded.add()
	if hold != nil {
		<-hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	defer func() {
		f.answered++
		f.responded.add()
	}()
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

func (f *fakeTranscribe) uploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeTranscribe) awaitUploads(t *testing.T, n int) {
	t.Helper()
	f.uploaded.await(t, fmt.Sprintf("the transcription route received too few uploads, want %d", n), func(count int) bool { return count >= n })
}

func (f *fakeTranscribe) upload(i int) transcribeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

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

func startRecording(p *page, form string) {
	p.t.Helper()
	var before int
	p.eval(`recorders.length`, &before)
	p.click(form + ` [aria-label=Dictate]`)
	p.await("a running recorder", fmt.Sprintf(`recorders.length === %d && recorders.at(-1).state === 'recording'`, before+1))
	checkDictationIcon(p, form, true)
}

func stopRecording(p *page, form string) {
	p.t.Helper()
	p.click(form + ` [aria-label=Dictate]`)
	p.awaitText(form+` .result`, "Transcribing")
	checkDictationIcon(p, form, false)
}

func awaitAnswerSeen(p *page, n int) {
	p.t.Helper()
	p.await("the transcription answer", fmt.Sprintf(`transcribeSettled >= %d`, n))
}

type composer struct {
	t        *testing.T
	p        *page
	a        *chatAPI
	view     chatView
	phone    bool
	releases []func()
	mic      string
	textarea string
	result   string
	draft    string
	outcome  string
	pressed  string
	idle     string
	uploads  int
	messages int
}

func forEachComposer(t *testing.T, check func(c *composer)) {
	t.Helper()
	for _, phone := range []bool{false, true} {
		t.Run(fmt.Sprintf("phone=%t", phone), func(t *testing.T) {
			t.Parallel()
			p, a, _ := openDictationPage(t, phone, fakeDictationScript)
			for _, view := range chatViews {
				p.selectChat(view, phone)
				c := &composer{t: t, p: p, a: a, view: view, phone: phone,
					mic: view.form + ` [aria-label=Dictate]`, textarea: view.form + ` textarea`, result: view.form + ` .result`,
					uploads: a.transcribe.uploads(), messages: a.count()}
				c.draft = `document.querySelector(` + quote(c.textarea) + `).value`
				c.outcome = `document.querySelector(` + quote(c.result) + `).textContent`
				c.pressed = `document.querySelector(` + quote(c.mic) + `).getAttribute('aria-pressed')`
				c.idle = `document.querySelector(` + quote(c.mic) + `).getAttribute('aria-pressed') === 'false' && !document.querySelector(` + quote(c.mic) + `).disabled`
				checkDictationIcon(p, view.form, false)
				check(c)
				for _, release := range c.releases {
					release()
				}
			}
		})
	}
}

func (c *composer) setDraft(text string) {
	c.p.eval(`(() => { const t = document.querySelector(`+quote(c.textarea)+`); t.value = `+quote(text)+`; t.focus(); t.setSelectionRange(7, 13); })()`, nil)
}

func (c *composer) startRecording() { startRecording(c.p, c.view.form) }

func (c *composer) stopRecording() { stopRecording(c.p, c.view.form) }

func (c *composer) holdTranscription() func() {
	release := sync.OnceFunc(c.a.transcribe.holdResponse())
	c.t.Cleanup(release)
	c.releases = append(c.releases, release)
	return release
}

func (c *composer) holdSend() func() {
	release := sync.OnceFunc(c.a.action.holdResponse())
	c.t.Cleanup(release)
	c.releases = append(c.releases, release)
	return release
}

func (c *composer) dictate(words string) {
	c.a.transcribe.answer(words)
	c.startRecording()
	c.p.click(c.mic)
	c.uploads++
}

func (c *composer) dictateAndAwait(words, draft string) {
	c.dictate(words)
	c.p.await("the transcription of "+words, c.draft+` === `+quote(draft))
}

func (c *composer) refuseAs(status int, code, message string) {
	c.a.transcribe.refuse(status, code, message)
	c.t.Cleanup(func() { c.a.transcribe.refuse(0, "", "") })
}

func (c *composer) assertDraftKeptAndIdle(draft string) {
	c.t.Helper()
	assertDictation(c.p, c.draft+` === `+quote(draft)+` && `+c.idle)
}

func (c *composer) assertSentMessage(step, text string) {
	c.t.Helper()
	c.messages++
	assertChatMessage(c.t, c.a, c.messages, c.view, step, text)
}

func (c *composer) assertUploads() {
	c.t.Helper()
	if got := c.a.transcribe.uploads(); got != c.uploads {
		c.t.Fatalf("%s: %d uploads, want %d", c.view.name, got, c.uploads)
	}
}

func TestBrowserDictationRecordsAudioAndUploadsIt(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.setDraft("Before SELECT after")
		c.a.transcribe.answer("hello world")
		c.startRecording()
		assertDictation(c.p, `recorders.at(-1).starts === 1 && recorders.at(-1).mimeType === 'audio/webm;codecs=opus' && mediaConstraints.audio === true`)
		c.holdTranscription()
		c.stopRecording()
		c.uploads++
		c.a.transcribe.awaitUploads(c.t, c.uploads)
		assertDictation(c.p, `recorders.at(-1).stops === 1 && tracks.at(-1).stopped`)
		if got := c.a.transcribe.upload(c.uploads - 1); got.ContentType != "application/json" || got.MIMEType != "audio/webm;codecs=opus" || string(got.Audio) != "voice" {
			c.t.Fatalf("%s upload = %+v", c.view.name, got)
		}
	})
}

func TestBrowserDictationKeepsTheDraftWhileTranscribing(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.setDraft("Before SELECT after")
		c.a.transcribe.answer("hello world")
		c.startRecording()
		c.holdTranscription()
		c.stopRecording()
		c.uploads++
		c.a.transcribe.awaitUploads(c.t, c.uploads)
		assertDictation(c.p, c.draft+` === 'Before SELECT after' && `+c.outcome+`.includes('Transcribing')`)
	})
}

func TestBrowserDictationAppendsTheTranscriptionAfterTheDraft(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.setDraft("Before SELECT after")
		c.dictate("hello world")
		c.p.await("the appended transcription", c.draft+` === 'Before SELECT after hello world' && `+c.outcome+` === ''`)
		assertDictation(c.p, c.idle)
	})
}

func TestBrowserDictationAppendsEveryTranscription(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.setDraft("Before SELECT after")
		c.dictateAndAwait("hello world", "Before SELECT after hello world")
		c.dictateAndAwait("and more", "Before SELECT after hello world and more")
		assertDictation(c.p, c.idle)
	})
}

func TestBrowserDictationIntoAnEmptyDraftHoldsOnlyTheWords(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.setDraft("")
		c.dictateAndAwait("Just the words.", "Just the words.")
	})
}

func TestBrowserDictationNeverSendsAMessage(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.setDraft("")
		c.dictateAndAwait("Just the words.", "Just the words.")
		if got := c.a.count(); got != c.messages {
			c.t.Fatalf("%s: recording sent a message: %d, want %d", c.view.name, got, c.messages)
		}
	})
}

func TestBrowserDictationReportsADeniedMicrophone(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.setDraft("Keep this draft")
		c.p.eval(`window.mediaError = 'NotAllowedError'`, nil)
		c.p.click(c.mic)
		c.p.awaitText(c.result, "Microphone permission denied")
		c.assertDraftKeptAndIdle("Keep this draft")
		c.p.eval(`window.mediaError = null`, nil)
	})
}

func TestBrowserDictationReportsARecordingWithNoAudio(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.setDraft("Keep this draft")
		c.p.eval(`window.silence = true`, nil)
		c.startRecording()
		c.p.click(c.mic)
		c.p.awaitText(c.result, "Nothing was recorded")
		c.assertDraftKeptAndIdle("Keep this draft")
		c.p.eval(`window.silence = false`, nil)
	})
}

func TestBrowserDictationShowsTheServicesTranscriptionRefusal(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		for _, failure := range []struct {
			status        int
			code, message string
		}{
			{http.StatusConflict, "transcription_off", "transcription is not configured on this service"},
			{http.StatusBadGateway, "transcription_failed", "the transcription server could not be reached"},
		} {
			c.setDraft("Keep this draft")
			c.refuseAs(failure.status, failure.code, failure.message)
			c.startRecording()
			c.p.click(c.mic)
			c.uploads++
			c.p.awaitText(c.result, failure.message)
			assertDictation(c.p, c.draft+` === 'Keep this draft' && `+c.idle+` && `+c.outcome+`.length > 0`)
		}
	})
}

func TestBrowserDictationReportsATranscriptionWithNoSpeech(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.setDraft("Keep this draft")
		c.dictate("   ")
		c.p.awaitText(c.result, "No speech was detected")
		c.assertDraftKeptAndIdle("Keep this draft")
		c.assertUploads()
	})
}

func TestBrowserDictationReportsARecorderFailure(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.setDraft("Keep this draft")
		c.startRecording()
		c.p.eval(`recorders.at(-1).error('device lost')`, nil)
		c.p.awaitText(c.result, "Recording failed")
		assertDictation(c.p, c.draft+` === 'Keep this draft' && `+c.idle+` && tracks.at(-1).stopped && recorders.at(-1).stops === 1`)
	})
}

func (c *composer) sendWhileRecording() (releaseSend func()) {
	c.setDraft("Keep this draft")
	c.startRecording()
	releaseSend = c.holdSend()
	c.p.click(c.view.form + ` [aria-label=Send]`)
	return releaseSend
}

func TestBrowserDictationSendWhileRecordingSendsTheDraftAndStopsTheRecorder(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.sendWhileRecording()
		c.assertSentMessage("send while recording", "Keep this draft")
		assertDictation(c.p, `recorders.at(-1).stops === 1 && tracks.at(-1).stopped && `+c.pressed+` === 'false'`)
	})
}

func TestBrowserDictationIgnoresDataFromARecorderStoppedBySend(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		releaseSend := c.sendWhileRecording()
		c.assertSentMessage("send while recording", "Keep this draft")
		c.p.awaitText(c.result, "Sending…")
		c.p.await("the stopped recorder's callbacks", `recorders.at(-1).delivered`)
		c.p.eval(`recorders.at(-1).data('late'); recorders.at(-1).finish()`, nil)
		assertDictation(c.p, c.outcome+` === 'Sending…'`)
		releaseSend()
		c.p.awaitText(c.result, c.view.result)
		c.p.await("cleared input after send", c.draft+` === ''`)
	})
}

func TestBrowserDictationUploadsNothingForARecordingStoppedBySend(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		releaseSend := c.sendWhileRecording()
		c.assertSentMessage("send while recording", "Keep this draft")
		releaseSend()
		c.p.awaitText(c.result, c.view.result)
		c.assertUploads()
	})
}

func (c *composer) sendWhileTranscribing() (releaseSend, releaseTranscription func()) {
	c.setDraft("Keep this draft")
	c.a.transcribe.answer("late words")
	releaseTranscription = c.holdTranscription()
	c.startRecording()
	c.stopRecording()
	c.uploads++
	c.a.transcribe.awaitUploads(c.t, c.uploads)
	releaseSend = c.holdSend()
	c.p.click(c.view.form + ` [aria-label=Send]`)
	return releaseSend, releaseTranscription
}

func TestBrowserDictationSendWhileTranscribingSendsTheDraft(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.sendWhileTranscribing()
		c.assertSentMessage("send while transcribing", "Keep this draft")
		c.p.awaitText(c.result, "Sending…")
	})
}

func TestBrowserDictationLeavesTheDraftAloneWhenATranscriptionArrivesAfterSend(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		answers := c.a.transcribe.answeredCount()
		releaseSend, releaseTranscription := c.sendWhileTranscribing()
		c.assertSentMessage("send while transcribing", "Keep this draft")
		c.p.awaitText(c.result, "Sending…")
		releaseTranscription()
		c.a.transcribe.awaitAnswered(c.t, answers+1)
		awaitAnswerSeen(c.p, c.uploads)
		assertDictation(c.p, c.draft+` === 'Keep this draft' && `+c.outcome+` === 'Sending…'`)
		releaseSend()
		c.p.awaitText(c.result, c.view.result)
		c.p.await("cleared input after the send", c.draft+` === '' && `+c.idle)
	})
}

func TestBrowserDictationAfterASendHoldsOnlyTheNextTranscription(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		answers := c.a.transcribe.answeredCount()
		releaseSend, releaseTranscription := c.sendWhileTranscribing()
		c.assertSentMessage("send while transcribing", "Keep this draft")
		releaseTranscription()
		c.a.transcribe.awaitAnswered(c.t, answers+1)
		awaitAnswerSeen(c.p, c.uploads)
		releaseSend()
		c.p.await("cleared input after the send", c.draft+` === '' && `+c.idle)
		c.dictateAndAwait("fresh words", "fresh words")
	})
}

func TestBrowserDictationSendsAnEditedTranscription(t *testing.T) {
	t.Parallel()
	forEachComposer(t, func(c *composer) {
		c.setDraft("")
		c.dictateAndAwait("fresh words", "fresh words")
		c.p.eval(`(() => { const t = document.querySelector(`+quote(c.textarea)+`); t.value += ' (edited)'; })()`, nil)
		if c.phone {
			c.p.run(chromedp.Focus(c.textarea, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
			if got := c.a.count(); got != c.messages {
				c.t.Fatalf("phone Enter sent: %d, want %d", got, c.messages)
			}
			c.p.click(c.view.form + ` [aria-label=Send]`)
		} else {
			c.p.run(chromedp.Focus(c.textarea, chromedp.ByQuery))
			c.p.await("focused desktop textarea", `document.activeElement === document.querySelector(`+quote(c.textarea)+`) && matchMedia('(pointer: fine)').matches`)
			c.p.run(chromedp.KeyEvent(kb.Enter))
		}
		c.assertSentMessage("edited transcription", "fresh words (edited)")
		c.p.awaitText(c.result, c.view.result)
		c.p.await("cleared input after Enter", c.draft+` === ''`)
	})
}

func TestBrowserDictationContactsOnlyTheServiceOrigin(t *testing.T) {
	t.Parallel()
	for _, phone := range []bool{false, true} {
		t.Run(fmt.Sprintf("phone=%t", phone), func(t *testing.T) {
			t.Parallel()
			p, a, foreign := openDictationPage(t, phone, fakeDictationScript)
			messages := 0
			for _, view := range chatViews {
				p.selectChat(view, phone)
				c := &composer{t: t, p: p, a: a, view: view, phone: phone,
					mic: view.form + ` [aria-label=Dictate]`, textarea: view.form + ` textarea`, result: view.form + ` .result`}
				c.draft = `document.querySelector(` + quote(c.textarea) + `).value`
				c.setDraft("")
				c.dictateAndAwait("hello", "hello")
				p.click(view.form + ` [aria-label=Send]`)
				messages++
				assertChatMessage(t, a, messages, view, "send", "hello")
				p.awaitText(c.result, view.result)
			}
			a.mu.Lock()
			var extra []string
			for _, path := range a.posts {
				if path != "/v1/transcribe" {
					extra = append(extra, path)
				}
			}
			a.mu.Unlock()
			if len(extra) != messages {
				t.Fatalf("requests other than transcriptions and messages: %v, want %d messages", extra, messages)
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
	f.responded.await(t, fmt.Sprintf("the transcription route answered too few uploads, want %d", n), func(count int) bool { return count >= n })
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

type pairedComposer struct {
	*composer
	other                chatView
	bMic, bText, bResult string
}

func forEachPairedComposer(t *testing.T, check func(c *pairedComposer)) {
	t.Helper()
	forEachComposer(t, func(c *composer) {
		other := chatViews[0]
		switch c.view.name {
		case "workstream":
			other = chatViews[1]
		case "session":
			other = chatView{name: "session", id: "w_005", form: "#session-form"}
		case "design":
			other = chatView{name: "design", id: "w_006", form: "#design-form"}
		}
		check(&pairedComposer{composer: c, other: other,
			bMic: other.form + ` [aria-label=Dictate]`, bText: other.form + ` textarea`, bResult: other.form + ` .result`})
	})
}

func (c *pairedComposer) openOther() { c.p.selectChat(c.other, c.phone) }

func (c *pairedComposer) openThis() { c.p.selectChat(c.view, c.phone) }

func (c *pairedComposer) leaveWhileRecording() {
	c.p.typeInto(c.textarea, "A draft")
	c.startRecording()
	c.p.eval(`window.aRecorder = recorders.at(-1); window.aTrack = tracks.at(-1)`, nil)
	c.openOther()
}

func (c *pairedComposer) writeOtherDraft() {
	c.p.eval(`document.querySelector(`+quote(c.bText)+`).value = 'B draft'; window.bOutcome = document.querySelector(`+quote(c.bResult)+`).textContent`, nil)
}

func (c *pairedComposer) assertOtherUntouched() {
	c.t.Helper()
	assertDictation(c.p, `document.querySelector(`+quote(c.bText)+`).value === 'B draft' && document.querySelector(`+quote(c.bResult)+`).textContent === bOutcome && document.querySelector(`+quote(c.bMic)+`).getAttribute('aria-pressed') === 'false'`)
}

func TestBrowserDictationStopsRecordingWhenTheConversationChanges(t *testing.T) {
	t.Parallel()
	forEachPairedComposer(t, func(c *pairedComposer) {
		c.leaveWhileRecording()
		assertDictation(c.p, `aRecorder.stops === 1 && aTrack.stopped && document.querySelector(`+quote(c.mic)+`).getAttribute('aria-pressed') === 'false'`)
	})
}

func TestBrowserDictationIgnoresLateDataFromARecorderLeftBehind(t *testing.T) {
	t.Parallel()
	forEachPairedComposer(t, func(c *pairedComposer) {
		c.leaveWhileRecording()
		c.writeOtherDraft()
		c.p.await("the stopped recorder's callbacks", `aRecorder.delivered`)
		c.p.eval(`aRecorder.data('late A'); aRecorder.finish(); aRecorder.error('late')`, nil)
		c.assertOtherUntouched()
	})
}

func TestBrowserDictationUploadsNothingForARecorderLeftBehind(t *testing.T) {
	t.Parallel()
	forEachPairedComposer(t, func(c *pairedComposer) {
		c.leaveWhileRecording()
		c.p.await("the stopped recorder's callbacks", `aRecorder.delivered`)
		c.p.eval(`aRecorder.data('late A'); aRecorder.finish()`, nil)
		c.assertUploads()
	})
}

func TestBrowserDictationIgnoresOldRecorderDataOnReturn(t *testing.T) {
	t.Parallel()
	forEachPairedComposer(t, func(c *pairedComposer) {
		c.leaveWhileRecording()
		c.p.await("the stopped recorder's callbacks", `aRecorder.delivered`)
		c.openThis()
		c.p.eval(`window.returnDraft = document.querySelector(`+quote(c.textarea)+`).value; window.returnOutcome = document.querySelector(`+quote(c.result)+`).textContent`, nil)
		c.p.eval(`aRecorder.data('old A on return'); aRecorder.finish()`, nil)
		assertDictation(c.p, `document.querySelector(`+quote(c.textarea)+`).value === returnDraft && document.querySelector(`+quote(c.result)+`).textContent === returnOutcome && document.querySelector(`+quote(c.mic)+`).getAttribute('aria-pressed') === 'false'`)
	})
}

func (c *pairedComposer) captureReturnState() {
	c.openOther()
	c.writeOtherDraft()
	c.openThis()
	c.p.eval(`window.returnDraft = document.querySelector(`+quote(c.textarea)+`).value`, nil)
}

func (c *pairedComposer) transcribeWhileAway() {
	c.a.transcribe.answer("answer for A")
	release := c.holdTranscription()
	c.startRecording()
	c.stopRecording()
	c.uploads++
	c.a.transcribe.awaitUploads(c.t, c.uploads)
	answers := c.a.transcribe.answeredCount()
	c.openOther()
	c.writeOtherDraft()
	release()
	c.a.transcribe.awaitAnswered(c.t, answers+1)
	awaitAnswerSeen(c.p, c.uploads)
}

func TestBrowserDictationTranscriptionForAnotherConversationLeavesTheOpenOneAlone(t *testing.T) {
	t.Parallel()
	forEachPairedComposer(t, func(c *pairedComposer) {
		c.p.typeInto(c.textarea, "A draft")
		c.transcribeWhileAway()
		c.assertOtherUntouched()
	})
}

func TestBrowserDictationReturnShowsTheDraftWithoutAStaleTranscription(t *testing.T) {
	t.Parallel()
	forEachPairedComposer(t, func(c *pairedComposer) {
		c.p.typeInto(c.textarea, "A draft")
		c.captureReturnState()
		c.transcribeWhileAway()
		c.openThis()
		assertDictation(c.p, `document.querySelector(`+quote(c.textarea)+`).value === returnDraft && document.querySelector(`+quote(c.result)+`).textContent === '' && document.querySelector(`+quote(c.mic)+`).getAttribute('aria-pressed') === 'false' && !document.querySelector(`+quote(c.mic)+`).disabled`)
	})
}

func TestBrowserDictationWorksAfterReturningToAConversation(t *testing.T) {
	t.Parallel()
	forEachPairedComposer(t, func(c *pairedComposer) {
		c.p.typeInto(c.textarea, "A draft")
		c.captureReturnState()
		c.transcribeWhileAway()
		c.openThis()
		c.dictate("second")
		c.p.await("a fresh transcription", `document.querySelector(`+quote(c.textarea)+`).value === (returnDraft ? returnDraft + ' ' : '') + 'second'`)
	})
}
