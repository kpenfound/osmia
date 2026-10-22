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

// chatAPI extends the ordinary fake API with the conversation kinds
// and records messages at their real endpoints, without running agents.
type chatAPI struct {
	base     *fakeAPI
	mu       sync.Mutex
	messages []chatMessage
	posts    []string
	entries  map[string][]map[string]any
	action   fakeAction
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
	// Set only the viewport metrics: EmulateViewport also resets touch and
	// pointer capabilities, which the keyboard and touch tests set explicitly.
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

func TestBrowserConversationWidthsAndSendIcons(t *testing.T) {
	for _, size := range []struct{ width, height int64 }{{1920, 1080}, {1440, 900}, {390, 844}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			p, _ := openChatPage(t, size.width, size.height)
			phone := size.width == 390
			for _, view := range chatViews {
				p.selectChat(view, phone)
				p.checkSendIcon(view.form)
				var m struct {
					Pane, Content, Left, Right, PaddingLeft, PaddingRight float64
					Overflow, Sidebar, Picker                             bool
				}
				p.eval(`(() => {
     const pane = document.querySelector('#main'), view = document.querySelector('.view:not([hidden])');
     const p = pane.getBoundingClientRect(), v = view.getBoundingClientRect(), s = getComputedStyle(view);
     const l = parseFloat(s.paddingLeft), r = parseFloat(s.paddingRight);
     return {Pane:p.width, Content:v.width-l-r, Left:v.left+l-p.left, Right:p.right-v.right+r,
      PaddingLeft:l,PaddingRight:r,Overflow:document.documentElement.scrollWidth > innerWidth || document.body.scrollWidth > innerWidth || pane.scrollWidth > pane.clientWidth,
      Sidebar:getComputedStyle(document.querySelector('#sidebar')).display !== 'none', Picker:getComputedStyle(document.querySelector('#picker-toggle')).display !== 'none'};
    })()`, &m)
				t.Logf("%s layout: %+v", view.name, m)
				if m.Overflow || m.Left <= 0 || m.Right <= 0 {
					t.Fatalf("%s has overflow or no gutter: %+v", view.name, m)
				}
				if phone {
					if m.Sidebar || !m.Picker || m.PaddingLeft != 12 || m.PaddingRight != 12 {
						t.Fatalf("phone layout: %+v", m)
					}
				} else if !m.Sidebar || m.Content < .75*m.Pane {
					t.Fatalf("desktop content is too narrow: %+v", m)
				}
				if view.name == "session" {
					continue
				}
				feed := `[data-view="` + view.name + `"] .feed`
				p.await("long owner and agent messages", `document.querySelectorAll(`+quote(feed+` .entry .text`)+`).length === 2`)
				var bubbles []struct {
					Kind, Max     string
					Width, Column float64
				}
				p.eval(`Array.from(document.querySelectorAll(`+quote(feed+` .entry .text`)+`), text => ({
     Kind:text.parentElement.dataset.kind, Max:getComputedStyle(text).maxWidth, Width:text.getBoundingClientRect().width,
     Column:text.closest('.feed').getBoundingClientRect().width
    }))`, &bubbles)
				for _, bubble := range bubbles {
					t.Logf("%s bubble: %+v", view.name, bubble)
					if size.width == 1920 && bubble.Width < .6*m.Content {
						t.Fatalf("long message is too narrow: %+v", bubble)
					}
					if phone && bubble.Kind == "message" && (bubble.Max != "92%" || bubble.Width > .92*bubble.Column+1) {
						t.Fatalf("phone bubble limit: %+v", bubble)
					}
				}
			}
			p.await("assets from the service origin", `performance.getEntriesByType('resource').every(r => new URL(r.name).origin === location.origin)`)
		})
	}
}

func assertChatMessage(t *testing.T, a *chatAPI, count int, view chatView, step, text string) {
	t.Helper()
	deadline := time.Now().Add(browserTimeout)
	for a.count() < count && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
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

func TestBrowserChatComposersClickAndKeyboard(t *testing.T) {
	// A container need not have a physical mouse. Give Chromium a fine
	// pointer (Blink's enum value 4) so this exercises desktop Enter handling.
	p, a := openChatPage(t, 1440, 900, chromedp.Flag("blink-settings", "primaryPointerType=4,availablePointerTypes=4"))
	p.await("a desktop fine pointer", `matchMedia('(pointer: fine)').matches`)
	count := 0
	for _, view := range chatViews {
		p.selectChat(view, false)
		textarea, button, result := view.form+" textarea", view.form+" button[type=submit]", view.form+" .result"
		// Observe the disabled icon and existing pending/result reporting while
		// the server is still processing a click.
		release := sync.OnceFunc(a.action.holdResponse())
		t.Cleanup(release)
		p.typeInto(textarea, "  Click the arrow  ")
		p.click(button)
		count++
		assertChatMessage(t, a, count, view, "click", "Click the arrow")
		p.await("the disabled in-flight arrow", `document.querySelector(`+quote(button)+`).disabled`)
		p.awaitText(result, "Sending…")
		p.checkSendIcon(view.form)
		release()
		p.awaitText(result, view.result)
		p.await("the enabled arrow and empty input", `!document.querySelector(`+quote(button)+`).disabled && document.querySelector(`+quote(textarea)+`).value === ''`)
		p.awaitText(`[data-view="`+view.name+`"] .feed`, "Click the arrow")

		p.typeInto(textarea, "First line")
		p.run(chromedp.Focus(textarea, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter, chromedp.KeyModifiers(input.ModifierShift)))
		p.typeInto(textarea, "Second line")
		p.await("Shift+Enter inserts a newline", `document.querySelector(`+quote(textarea)+`).value === 'First line\nSecond line'`)
		// A composing Enter must not submit even on a desktop pointer.
		p.eval(`document.querySelector(`+quote(textarea)+`).dispatchEvent(new KeyboardEvent('keydown',{key:'Enter',isComposing:true,bubbles:true,cancelable:true}))`, nil)
		if got := awaitQuiet(t, a.count, 200*time.Millisecond); got != count {
			t.Fatalf("%s Shift+Enter or IME submitted: %d messages, want %d", view.name, got, count)
		}
		p.run(chromedp.Focus(textarea, chromedp.ByQuery))
		p.await(view.name+" focused desktop composer", `document.activeElement === document.querySelector(`+quote(textarea)+`) && matchMedia('(pointer: fine)').matches`)
		settleEnter := sync.OnceFunc(a.action.holdResponse())
		t.Cleanup(settleEnter)
		p.run(chromedp.KeyEvent(kb.Enter))
		count++
		assertChatMessage(t, a, count, view, "Enter", "First line\nSecond line")
		p.awaitText(result, "Sending…")
		settleEnter()
		p.awaitText(result, view.result)
		p.await("the cleared input after Enter", `document.querySelector(`+quote(textarea)+`).value === ''`)

		a.action.failNext("The agent is busy.")
		p.typeInto(textarea, "Keep this draft")
		p.click(button)
		count++
		assertChatMessage(t, a, count, view, "refused click", "Keep this draft")
		p.awaitText(result, "The agent is busy.")
		p.await("the draft and enabled icon after refusal", `document.querySelector(`+quote(textarea)+`).value === 'Keep this draft' && !document.querySelector(`+quote(button)+`).disabled`)
		p.checkSendIcon(view.form)
		a.action.failNext("")
	}
}

func TestBrowserChatTouchEnterInsertsNewline(t *testing.T) {
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
