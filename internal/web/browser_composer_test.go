package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

type chatAPI struct {
	base       *fakeAPI
	mu         sync.Mutex
	messages   []chatMessage
	posts      []string
	entries    map[string][]map[string]any
	action     fakeAction
	transcribe fakeTranscribe
	received   counter
}

type chatMessage struct {
	Path string
	Body map[string]string
}

type chatView struct{ name, id, form, endpoint, result string }

var chatViews = []chatView{
	{"workstream", "w_000", `[data-workstream="w_000"] form[data-field=send]`, "/v1/conversation/w_000", "Sent; the chief of staff answers in its next turn (pending)."},
	{"workgroup", "g_test", `[data-view="workgroup"] form[data-field=send]`, "/v1/workgroups/g_test/conversation", "Sent; the chief of staff answers in its next turn (pending)."},
	{"session", "w_001", "#session-form", "/v1/sessions/w_001/message", "Sent; the agent answers in its next turn (pending)."},
	{"design", "w_002", "#design-form", "/v1/design/w_002/message", "Sent; the design agent answers in its next turn (pending)."},
}

func newChatAPI() *chatAPI {
	a := &chatAPI{base: newFakeAPI(5), entries: make(map[string][]map[string]any)}
	a.transcribe.url = "http://whisper.test/v1/audio/transcriptions"
	text := strings.Repeat("The conversation should use the available space beside the workstream picker. This is a longer message with several sentences so the owner can read the full explanation comfortably. ", 8)
	for _, id := range []string{"w_000", "w_002", "g_test"} {
		a.entries[id] = []map[string]any{
			{"turn": "owner", "kind": "message", "text": text, "state": "done", "at": "2026-01-01T00:00:00Z"},
			{"turn": "agent", "kind": "response", "text": text, "state": "done", "at": "2026-01-01T00:00:01Z"},
		}
	}
	return a
}

func (a *chatAPI) count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.messages) }

func (a *chatAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		recorded := httptest.NewRecorder()
		a.base.handleStatus(recorded, r)
		var status map[string]any
		if err := json.Unmarshal(recorded.Body.Bytes(), &status); err != nil {
			panic(err)
		}
		streams := status["workstreams"].([]any)
		session := streams[1].(map[string]any)
		session["kind"], session["state"], session["profile"], session["title"] = "session", "idle", "fake", "Exploration"
		streams[2].(map[string]any)["kind"] = "design"
		group := map[string]any{"id": "g_test", "members": []string{"w_003", "w_004"}, "edges": []any{}}
		streams[4].(map[string]any)["workgroup"] = group
		streams[3].(map[string]any)["workgroup"] = group
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("/v1/feed/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/feed/")
		a.mu.Lock()
		defer a.mu.Unlock()
		entries := a.entries[id]
		if entries == nil {
			entries = []map[string]any{}
		}
		json.NewEncoder(w).Encode(map[string]any{"workstream": id, "entries": entries})
	})
	mux.HandleFunc("/v1/workgroups/g_test/feed", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"workgroup": "g_test", "entries": a.entries["g_test"], "members": []any{}})
	})
	mux.HandleFunc("/v1/design/w_002/conversation", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"entries": a.entries["w_002"]})
	})
	mux.HandleFunc("/v1/sessions/w_001/changes", handleJSON(`{"changes":"","commits":[]}`))
	mux.HandleFunc("/v1/config", a.transcribe.handleConfig)
	mux.HandleFunc("/v1/transcribe", a.transcribe.handle)
	for _, view := range chatViews {
		mux.HandleFunc(view.endpoint, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "expected POST", http.StatusMethodNotAllowed)
				return
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			a.mu.Lock()
			a.messages = append(a.messages, chatMessage{r.URL.Path, body})
			entry := map[string]any{"turn": fmt.Sprintf("sent-%d", len(a.messages)), "kind": "message", "text": body["text"], "state": "pending", "at": "2026-01-01T00:00:02Z"}
			if view.name != "session" {
				a.entries[view.id] = append(a.entries[view.id], entry)
			}
			a.mu.Unlock()
			a.received.add()
			encoded, _ := json.Marshal(entry)
			a.action.serve(w, r, string(encoded))
		})
	}
	mux.Handle("/", a.base.handler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			a.mu.Lock()
			a.posts = append(a.posts, r.URL.Path)
			a.mu.Unlock()
		}
		mux.ServeHTTP(w, r)
	})
}

func openChatPage(t *testing.T, width, height int64, browserOptions ...chromedp.ExecAllocatorOption) (*page, *chatAPI) {
	t.Helper()
	a := newChatAPI()
	server := httptest.NewServer(a.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t, browserOptions...)
	p.run(emulation.SetDeviceMetricsOverride(width, height, 1, false), chromedp.Navigate(server.URL+"/"))
	p.await("the conversations", `document.querySelectorAll('#workstream-list [data-select]').length === 5`)
	return p, a
}

func (p *page) selectChat(view chatView, phone bool) {
	p.t.Helper()
	if phone {
		p.click("#picker-toggle")
		p.awaitVisible("the picker", "#sidebar")
	}
	if view.name == "workgroup" {
		p.click(`[data-select-workgroup="` + view.id + `"]`)
	} else {
		p.click(`[data-select="` + view.id + `"]`)
	}
	p.awaitVisible(view.name+" conversation", `[data-view="`+view.name+`"]`)
	p.awaitVisible("the composer", view.form)
}

func (p *page) checkSendIcon(form string) {
	p.t.Helper()
	var valid bool
	p.eval(`(() => {
  const form = document.querySelector(`+quote(form)+`), box = form.querySelector('.box'), button = form.querySelector('button[type=submit]');
  const svg = button.querySelector('svg'), path = svg?.querySelector('path');
  const b = button.getBoundingClientRect(), c = box.getBoundingClientRect();
  return button.getAttribute('aria-label') === 'Send' && button.innerText.trim() === '' &&
   svg?.namespaceURI === 'http://www.w3.org/2000/svg' && svg.getAttribute('aria-hidden') === 'true' &&
   path?.getAttribute('d') === 'M12 19V5m-7 7 7-7 7 7' && svg.getBoundingClientRect().width > 0 &&
   box.contains(button) && b.width > 0 && b.height > 0 && b.left > c.left && b.right < c.right && b.top > c.top && b.bottom < c.bottom;
 })()`, &valid)
	if !valid {
		p.t.Fatal("the send control must be an accessible inline up arrow inside the composer box")
	}
}

func forEachChatSize(t *testing.T, check func(t *testing.T, p *page, phone bool)) {
	t.Helper()
	for _, size := range []struct{ width, height int64 }{{1920, 1080}, {1440, 900}, {390, 844}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			p, _ := openChatPage(t, size.width, size.height)
			check(t, p, size.width == 390)
		})
	}
}

func TestBrowserSendIconsMatchOnEveryConversation(t *testing.T) {
	t.Parallel()
	forEachChatSize(t, func(t *testing.T, p *page, phone bool) {
		for _, view := range chatViews {
			p.selectChat(view, phone)
			p.checkSendIcon(view.form)
		}
	})
}

func TestBrowserConversationHasAGutterAndNeverOverflows(t *testing.T) {
	t.Parallel()
	forEachChatSize(t, func(t *testing.T, p *page, phone bool) {
		for _, view := range chatViews {
			p.selectChat(view, phone)
			m := readConversationLayout(p)
			if m.Overflow || m.Left <= 0 || m.Right <= 0 {
				t.Fatalf("%s has overflow or no gutter: %+v", view.name, m)
			}
		}
	})
}

func TestBrowserConversationUsesTheSpaceTheScreenAllows(t *testing.T) {
	t.Parallel()
	forEachChatSize(t, func(t *testing.T, p *page, phone bool) {
		for _, view := range chatViews {
			p.selectChat(view, phone)
			m := readConversationLayout(p)
			if phone {
				if m.Sidebar || !m.Picker || m.PaddingLeft != 12 || m.PaddingRight != 12 {
					t.Fatalf("%s phone layout: %+v", view.name, m)
				}
			} else if !m.Sidebar || m.Content < .75*m.Pane {
				t.Fatalf("%s desktop content is too narrow: %+v", view.name, m)
			}
		}
	})
}

func TestBrowserLongMessagesHoldTheReadingWidth(t *testing.T) {
	t.Parallel()
	forEachChatSize(t, func(t *testing.T, p *page, phone bool) {
		for _, view := range chatViews {
			if view.name == "session" {
				continue
			}
			p.selectChat(view, phone)
			for _, bubble := range readBubbles(p, view) {
				if !phone && (bubble.Width > bubble.Cap+1 || bubble.Width < .5*bubble.Cap) {
					t.Fatalf("%s long message is not held to the reading width: %+v", view.name, bubble)
				}
				if phone && bubble.Kind == "message" && (bubble.Max != "92%" || bubble.Width > .92*bubble.Column+1) {
					t.Fatalf("%s phone bubble limit: %+v", view.name, bubble)
				}
			}
		}
	})
}

func TestBrowserConversationLoadsAssetsOnlyFromTheServiceOrigin(t *testing.T) {
	t.Parallel()
	forEachChatSize(t, func(t *testing.T, p *page, phone bool) {
		for _, view := range chatViews {
			p.selectChat(view, phone)
		}
		p.await("assets from the service origin", `performance.getEntriesByType('resource').every(r => new URL(r.name).origin === location.origin)`)
	})
}

type conversationLayout struct {
	Pane, Content, Left, Right, PaddingLeft, PaddingRight float64
	Overflow, Sidebar, Picker                             bool
}

func readConversationLayout(p *page) conversationLayout {
	var m conversationLayout
	p.eval(`(() => {
     const pane = document.querySelector('#main'), view = document.querySelector('.view:not([hidden])');
     const p = pane.getBoundingClientRect(), v = view.getBoundingClientRect(), s = getComputedStyle(view);
     const l = parseFloat(s.paddingLeft), r = parseFloat(s.paddingRight);
     return {Pane:p.width, Content:v.width-l-r, Left:v.left+l-p.left, Right:p.right-v.right+r,
      PaddingLeft:l,PaddingRight:r,Overflow:document.documentElement.scrollWidth > innerWidth || document.body.scrollWidth > innerWidth || pane.scrollWidth > pane.clientWidth,
      Sidebar:getComputedStyle(document.querySelector('#sidebar')).display !== 'none', Picker:getComputedStyle(document.querySelector('#picker-toggle')).display !== 'none'};
    })()`, &m)
	return m
}

type bubbleSize struct {
	Kind, Max          string
	Width, Column, Cap float64
}

func readBubbles(p *page, view chatView) []bubbleSize {
	feed := `[data-view="` + view.name + `"] .feed`
	p.await("long owner and agent messages", `document.querySelectorAll(`+quote(feed+` .entry .text`)+`).length === 2`)
	var bubbles []bubbleSize
	p.eval(`Array.from(document.querySelectorAll(`+quote(feed+` .entry .text`)+`), text => {
     const probe = document.createElement('div');
     probe.style.width = '70ch';
     (text.querySelector('.markdown') || text).append(probe);
     const cap = probe.getBoundingClientRect().width;
     probe.remove();
     return {
      Kind:text.parentElement.dataset.kind, Max:getComputedStyle(text).maxWidth, Width:(text.querySelector('.markdown p') || text).getBoundingClientRect().width,
      Column:text.closest('.feed').getBoundingClientRect().width, Cap:cap
     };
    })`, &bubbles)
	return bubbles
}

func assertChatMessage(t *testing.T, a *chatAPI, count int, view chatView, step, text string) {
	t.Helper()
	a.received.wait(func(n int) bool { return n >= count })
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.messages) != count {
		t.Fatalf("%s %s: got %d messages, want %d at %s; received %d POSTs: %v; messages: %+v",
			view.name, step, len(a.messages), count, view.endpoint, len(a.posts), a.posts, a.messages)
	}
	got := a.messages[count-1]
	if got.Path != view.endpoint || len(got.Body) != 1 || got.Body["text"] != text {
		t.Fatalf("%s %s: message = %+v, want %s with text %q; received %d messages and %d POSTs: %v",
			view.name, step, got, view.endpoint, text, len(a.messages), len(a.posts), a.posts)
	}
}

type chatComposer struct {
	t        *testing.T
	p        *page
	a        *chatAPI
	view     chatView
	textarea string
	button   string
	result   string
	messages int
}

func forEachDesktopChatComposer(t *testing.T, check func(c *chatComposer)) {
	t.Helper()
	p, a := openChatPage(t, 1440, 900, chromedp.Flag("blink-settings", "primaryPointerType=4,availablePointerTypes=4"))
	p.await("a desktop fine pointer", `matchMedia('(pointer: fine)').matches`)
	for _, view := range chatViews {
		p.selectChat(view, false)
		check(&chatComposer{t: t, p: p, a: a, view: view, textarea: view.form + " textarea", button: view.form + " button[type=submit]", result: view.form + " .result", messages: a.count()})
	}
}

func (c *chatComposer) holdSend() func() {
	release := sync.OnceFunc(c.a.action.holdResponse())
	c.t.Cleanup(release)
	return release
}

func (c *chatComposer) assertSent(step, text string) {
	c.t.Helper()
	c.messages++
	assertChatMessage(c.t, c.a, c.messages, c.view, step, text)
}

func (c *chatComposer) focusTextarea() {
	c.p.run(chromedp.Focus(c.textarea, chromedp.ByQuery))
	c.p.await(c.view.name+" focused desktop composer", `document.activeElement === document.querySelector(`+quote(c.textarea)+`) && matchMedia('(pointer: fine)').matches`)
}

func (c *chatComposer) typeTwoLines() {
	c.p.typeInto(c.textarea, "First line")
	c.p.run(chromedp.Focus(c.textarea, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter, chromedp.KeyModifiers(input.ModifierShift)))
	c.p.typeInto(c.textarea, "Second line")
}

func TestBrowserChatClickSendsTheTrimmedDraft(t *testing.T) {
	t.Parallel()
	forEachDesktopChatComposer(t, func(c *chatComposer) {
		release := c.holdSend()
		c.p.typeInto(c.textarea, "  Click the arrow  ")
		c.p.click(c.button)
		c.assertSent("click", "Click the arrow")
		release()
	})
}

func TestBrowserChatSendInFlightDisablesTheArrowAndShowsSending(t *testing.T) {
	t.Parallel()
	forEachDesktopChatComposer(t, func(c *chatComposer) {
		release := c.holdSend()
		c.p.typeInto(c.textarea, "Click the arrow")
		c.p.click(c.button)
		c.p.await("the disabled in-flight arrow", `document.querySelector(`+quote(c.button)+`).disabled`)
		c.p.awaitText(c.result, "Sending…")
		c.p.checkSendIcon(c.view.form)
		release()
	})
}

func TestBrowserChatCompletedSendClearsTheInputAndShowsTheMessage(t *testing.T) {
	t.Parallel()
	forEachDesktopChatComposer(t, func(c *chatComposer) {
		release := c.holdSend()
		c.p.typeInto(c.textarea, "Click the arrow")
		c.p.click(c.button)
		release()
		c.p.awaitText(c.result, c.view.result)
		c.p.await("the enabled arrow and empty input", `!document.querySelector(`+quote(c.button)+`).disabled && document.querySelector(`+quote(c.textarea)+`).value === ''`)
		c.p.awaitText(`[data-view="`+c.view.name+`"] .feed`, "Click the arrow")
	})
}

func TestBrowserChatShiftEnterInsertsANewlineWithoutSending(t *testing.T) {
	t.Parallel()
	forEachDesktopChatComposer(t, func(c *chatComposer) {
		c.typeTwoLines()
		c.p.await("Shift+Enter inserts a newline", `document.querySelector(`+quote(c.textarea)+`).value === 'First line\nSecond line'`)
		if got := awaitQuiet(t, c.a.count, 200*time.Millisecond); got != c.messages {
			t.Fatalf("%s Shift+Enter submitted: %d messages, want %d", c.view.name, got, c.messages)
		}
	})
}

func TestBrowserChatEnterDuringCompositionDoesNotSend(t *testing.T) {
	t.Parallel()
	forEachDesktopChatComposer(t, func(c *chatComposer) {
		c.p.typeInto(c.textarea, "Composing")
		c.p.eval(`document.querySelector(`+quote(c.textarea)+`).dispatchEvent(new KeyboardEvent('keydown',{key:'Enter',isComposing:true,bubbles:true,cancelable:true}))`, nil)
		if got := awaitQuiet(t, c.a.count, 200*time.Millisecond); got != c.messages {
			t.Fatalf("%s IME Enter submitted: %d messages, want %d", c.view.name, got, c.messages)
		}
	})
}

func TestBrowserChatEnterSendsTheDraft(t *testing.T) {
	t.Parallel()
	forEachDesktopChatComposer(t, func(c *chatComposer) {
		c.typeTwoLines()
		c.focusTextarea()
		release := c.holdSend()
		c.p.run(chromedp.KeyEvent(kb.Enter))
		c.assertSent("Enter", "First line\nSecond line")
		c.p.awaitText(c.result, "Sending…")
		release()
		c.p.awaitText(c.result, c.view.result)
		c.p.await("the cleared input after Enter", `document.querySelector(`+quote(c.textarea)+`).value === ''`)
	})
}

func TestBrowserChatRefusedSendKeepsTheDraftAndEnablesTheArrow(t *testing.T) {
	t.Parallel()
	forEachDesktopChatComposer(t, func(c *chatComposer) {
		c.a.action.failNext("The agent is busy.")
		c.p.typeInto(c.textarea, "Keep this draft")
		c.p.click(c.button)
		c.assertSent("refused click", "Keep this draft")
		c.p.awaitText(c.result, "The agent is busy.")
		c.p.await("the draft and enabled icon after refusal", `document.querySelector(`+quote(c.textarea)+`).value === 'Keep this draft' && !document.querySelector(`+quote(c.button)+`).disabled`)
		c.p.checkSendIcon(c.view.form)
		c.a.action.failNext("")
	})
}

func TestBrowserChatTouchEnterInsertsNewline(t *testing.T) {
	t.Parallel()
	p, a := openChatPage(t, 390, 844)
	p.run(chromedp.ActionFunc(func(ctx context.Context) error { return emulation.SetTouchEmulationEnabled(true).Do(ctx) }))
	p.await("a touch pointer", `matchMedia('(pointer: coarse)').matches && !matchMedia('(pointer: fine)').matches`)
	for _, view := range chatViews {
		p.selectChat(view, true)
		p.typeInto(view.form+" textarea", "Touch message")
		p.run(chromedp.KeyEvent(kb.Enter))
		p.await("touch Enter inserts a newline", `document.querySelector(`+quote(view.form+" textarea")+`).value === 'Touch message\n'`)
	}
	if got := awaitQuiet(t, a.count, 200*time.Millisecond); got != 0 {
		t.Fatalf("touch Enter sent %d messages", got)
	}
}
