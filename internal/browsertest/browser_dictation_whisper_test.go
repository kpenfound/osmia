package browsertest

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/servicetest"
)

// fakeMedia stands in for the microphone and for MediaRecorder in the page:
// every recording yields one chunk of the bytes "voice".
const fakeMedia = `
 window.recorders = [];
 Object.defineProperty(navigator, 'mediaDevices', {configurable: true, value: {
  getUserMedia: async () => ({getTracks: () => [{stop() {}}]}),
 }});
 window.MediaRecorder = class {
  static isTypeSupported(type) { return type === 'audio/webm;codecs=opus'; }
  constructor(stream, options = {}) { recorders.push(this); this.mimeType = options.mimeType || ''; this.state = 'inactive'; }
  start() { this.state = 'recording'; }
  stop() {
   this.state = 'inactive';
   setTimeout(() => { this.ondataavailable({data: new Blob(['voice'], {type: this.mimeType})}); this.onstop(); }, 0);
  }
 };
`

// whisperRequest is what the fake Whisper server saw in one request.
type whisperRequest struct {
	path          string
	contentType   string
	model         string
	filename      string
	fileMediaType string
	audio         string
	authorization string
}

// fakeWhisper is an OpenAI-compatible transcription server on loopback that
// answers each request with the next of its texts.
type fakeWhisper struct {
	*httptest.Server
	mu       sync.Mutex
	texts    []string
	requests []whisperRequest
}

func newFakeWhisper(t *testing.T, texts ...string) *fakeWhisper {
	t.Helper()
	f := &fakeWhisper{texts: texts}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen := whisperRequest{path: r.URL.Path, contentType: r.Header.Get("Content-Type"), authorization: r.Header.Get("Authorization")}
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			seen.model = r.FormValue("model")
			if files := r.MultipartForm.File["file"]; len(files) == 1 {
				seen.filename = files[0].Filename
				seen.fileMediaType = files[0].Header.Get("Content-Type")
				if file, err := files[0].Open(); err == nil {
					audio, _ := io.ReadAll(file)
					file.Close()
					seen.audio = string(audio)
				}
			}
		}
		f.mu.Lock()
		n := len(f.requests)
		f.requests = append(f.requests, seen)
		f.mu.Unlock()
		text := ""
		if n < len(f.texts) {
			text = f.texts[n]
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":` + quote(text) + `}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWhisper) seen() []whisperRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]whisperRequest(nil), f.requests...)
}

// dictationPage starts the real service with its web listener and the extra
// top-level configuration, and opens the served conversation with fake media.
func dictationPage(t *testing.T, prefix, transcribe string) (*page, string, *Client) {
	t.Helper()
	opts, _ := servicetest.ConversationFixture(t, prefix)
	withWeb(t, opts, "127.0.0.1:0")
	if transcribe != "" {
		top, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
		must(t, err)
		_, err = top.WriteString("[transcribe]\n" + transcribe)
		must(t, err)
		must(t, top.Close())
	}
	s, c := start(t, opts)
	p := openBrowser(t)
	p.Run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(fakeMedia).Do(ctx)
		return err
	}))
	p.Run(chromedp.EmulateViewport(1440, 900), chromedp.Navigate("http://"+s.WebAddr()+"/#workstream="+string(stream)))
	form := `[data-workstream="` + string(stream) + `"] form[data-field=send]`
	p.Await("the conversation composer", `document.querySelector('[data-view=workstream]')?.hidden === false && document.querySelector(`+quote(form)+`) !== null && isSecureContext`)
	return p, form, c
}

func TestBrowserWhisperDictationFillsAndAppendsWithoutSending(t *testing.T) {
	t.Parallel()
	const first, second = "Please keep the upload API stable.", "Then ship the review."
	whisper := newFakeWhisper(t, first, second)
	p, form, c := dictationPage(t, "dictation-whisper-", "url = "+quote(whisper.URL+"/v1/audio/transcriptions")+"\nmodel = \"tiny-test\"\n")
	mic, area := form+` [aria-label=Dictate]`, form+` textarea`
	p.Await("the visible microphone immediately before Send", `(() => {
	 const mic = document.querySelector(`+quote(mic)+`), send = document.querySelector(`+quote(form+" [aria-label=Send]")+`);
	 return mic && !mic.hidden && mic.getBoundingClientRect().width > 0 && mic.nextElementSibling === send;
	})()`)

	record := func(want string) {
		t.Helper()
		before := len(whisper.seen())
		p.Click(mic)
		p.Await("recording", `recorders.at(-1)?.state === 'recording' && document.querySelector(`+quote(mic)+`).getAttribute('aria-pressed') === 'true'`)
		p.Click(mic)
		p.Await("the transcribed draft", `document.querySelector(`+quote(area)+`).value === `+quote(want)+` && document.querySelector(`+quote(mic)+`).getAttribute('aria-pressed') === 'false' && !document.querySelector(`+quote(mic)+`).disabled`)
		if got := len(whisper.seen()); got != before+1 {
			t.Fatalf("Whisper requests: %d, want %d", got, before+1)
		}
	}
	record(first)

	// A selection over the whole draft is never replaced; the next text
	// follows it after whitespace.
	p.Run(chromedp.Focus(area, chromedp.ByQuery))
	p.Eval(`(() => { const a = document.querySelector(`+quote(area)+`); a.setSelectionRange(0, a.value.length); })()`, nil)
	record(first + " " + second)

	requests := whisper.seen()
	if len(requests) != 2 {
		t.Fatalf("Whisper requests: %+v", requests)
	}
	for _, r := range requests {
		mediaType, _, err := mime.ParseMediaType(r.contentType)
		if r.path != "/v1/audio/transcriptions" || err != nil || mediaType != "multipart/form-data" {
			t.Fatalf("not a multipart transcription request: %+v", r)
		}
		if r.model != "tiny-test" || r.audio != "voice" || !strings.HasSuffix(r.filename, ".webm") || !strings.HasPrefix(r.fileMediaType, "audio/webm") {
			t.Fatalf("multipart request lacks the audio file or the configured model: %+v", r)
		}
	}
	p.Mu.Lock()
	requested := append([]string(nil), p.Requested...)
	p.Mu.Unlock()
	for _, url := range requested {
		if strings.HasPrefix(url, whisper.URL) {
			t.Fatalf("the page contacted the Whisper server itself: %s", url)
		}
	}

	conversation, err := c.Conversation(context.Background(), stream)
	must(t, err)
	if len(conversation.Entries) != 0 {
		t.Fatalf("dictation sent a message before Send: %+v", conversation.Entries)
	}
	p.Click(form + ` [aria-label=Send]`)
	p.AwaitText(form+" .result", "Sent; the chief of staff answers in its next turn")
	p.Await("cleared draft", `document.querySelector(`+quote(area)+`).value === ''`)
	conversation, err = c.Conversation(context.Background(), stream)
	must(t, err)
	if len(conversation.Entries) != 1 || conversation.Entries[0].Kind != "message" || conversation.Entries[0].Text != first+" "+second {
		t.Fatalf("the sent draft: %+v", conversation.Entries)
	}
	if len(whisper.seen()) != 2 {
		t.Fatalf("sending transcribed again: %+v", whisper.seen())
	}
}

func TestBrowserNoMicrophoneWithoutTranscription(t *testing.T) {
	t.Parallel()
	p, form, _ := dictationPage(t, "dictation-off-", "")
	p.Await("the composer without a microphone", `(() => {
	 const mic = document.querySelector(`+quote(form+" [aria-label=Dictate]")+`);
	 return mic && mic.hidden && getComputedStyle(mic).display === 'none' && !!document.querySelector(`+quote(form+" [aria-label=Send]")+`);
	})()`)
}
