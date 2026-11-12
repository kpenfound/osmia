package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"
)

type decisionRequest struct {
	Path, Method string
	Body         map[string]any
}

type decisionButtonFixture struct {
	api      *fakeAPI
	p        *page
	action   fakeAction
	mu       sync.Mutex
	requests []decisionRequest
	packet   string
}

func newDecisionButtonFixture(t *testing.T, entry string) *decisionButtonFixture {
	t.Helper()
	f := &decisionButtonFixture{api: newFakeAPI(1), packet: `{"packet":{"revision":{"spec":1,"plan":1},"dissent":[]}}`}
	f.api.setInbox(entry)
	base := f.api.handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			f.mu.Lock()
			f.requests = append(f.requests, decisionRequest{Path: r.URL.Path, Method: r.Method, Body: body})
			f.mu.Unlock()
			f.action.serve(w, r, `{"detail":"Decision recorded.","upstream":{"remote":"upstream","branch":"main"}}`)
		} else if strings.HasPrefix(r.URL.Path, "/v1/packet/") {
			f.mu.Lock()
			packet := f.packet
			f.mu.Unlock()
			writeJSON(w, packet)
		} else {
			base.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.p = openBrowser(t)
	f.p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	f.p.await("the live connection", `document.body.dataset.connection === 'live'`)
	f.p.click(`[data-select="w_000"]`)
	return f
}

func (f *decisionButtonFixture) got() []decisionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]decisionRequest(nil), f.requests...)
}

func assertDecisionButtons(p *page, card string, labels map[string]string) {
	p.t.Helper()
	p.awaitVisible("decision buttons", card+"[data-decision-choice]")
	var got map[string]string
	p.eval(`Object.fromEntries([...document.querySelectorAll(`+quote(card+"[data-decision-choice]")+`)].map(b => [b.dataset.decisionChoice,b.textContent]))`, &got)
	if !reflect.DeepEqual(got, labels) {
		p.t.Fatalf("buttons: %v, want %v", got, labels)
	}
	var valid bool
	p.eval(`(() => {
  const card = document.querySelector(`+quote(card)+`);
  const note = card.querySelector('[name=note]');
  return !card.querySelector('select[name=decision]') && !card.textContent.includes('Choose a decision') &&
   ![...card.querySelectorAll('button')].some(b => b.textContent === 'Decide') &&
   [...card.querySelectorAll('[data-decision-choice]')].every(b => !!(note.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING));
 })()`, &valid)
	if !valid {
		p.t.Fatal("decision controls must be buttons below the note")
	}
}

func assertDecisionRequest(t *testing.T, f *decisionButtonFixture, path string, body map[string]any) {
	t.Helper()
	got := f.got()
	want := decisionRequest{Path: path, Method: "POST", Body: body}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("requests: %+v, want %+v", got, want)
	}
}

func TestBrowserDecisionButtonsSendOfferedRulings(t *testing.T) {
	for _, tc := range []struct {
		kind, choice, label string
		note                bool
	}{
		{"contested", "review", "Review the unit again", true}, {"contested", "revise", "Revise the unit", true},
		{"amendment", "approve", "Approve", true}, {"amendment", "reject", "Reject", false},
		{"amendment", "round", "Debate another round", false}, {"amendment", "overrule", "Overrule the objections", true},
		{"base", "upstream", "Continue from upstream", false},
	} {
		t.Run(tc.kind+"/"+tc.choice, func(t *testing.T) {
			path, pin := "/v1/"+tc.kind+"/w_000/1", map[string]any{"packet": float64(3)}
			if tc.kind == "contested" {
				path, pin = "/v1/contested/w_000/index", map[string]any{}
			} else if tc.kind == "base" {
				path, pin = "/v1/base/w_000/upstream", map[string]any{"base": "w_parent"}
			}
			encodedPin, err := json.Marshal(pin)
			if err != nil {
				t.Fatal(err)
			}
			entry := fmt.Sprintf(`{"kind":%q,"project":"p_fake","workstream":"w_000","number":0,"unit":"index","amendment":"1","options":[%q],"asked":[],"answer":{"method":"POST","path":%q,"body":%s}}`, tc.kind, tc.choice, path, encodedPin)
			f := newDecisionButtonFixture(t, entry)
			card := `[data-decision][data-kind="` + tc.kind + `"] `
			assertDecisionButtons(f.p, card, map[string]string{tc.choice: tc.label})
			if tc.kind == "contested" {
				f.p.click(card + `[data-decision-choice="` + tc.choice + `"]`)
				f.p.awaitText("#inbox-result", "Give a note for the ruling.")
				if len(f.got()) != 0 {
					t.Fatal("empty contested note sent a request")
				}
			}
			if tc.note {
				f.p.typeInto(card+`[name=note]`, "  Keep the contract.  ")
			}
			body := pin
			if tc.kind != "base" {
				body["decision"] = tc.choice
			}
			if tc.note {
				body["note"] = "Keep the contract."
			}
			f.p.click(card + `[data-decision-choice="` + tc.choice + `"]`)
			f.p.awaitGone("the ruled card", card)
			f.p.await("the recorded outcome", `document.querySelector('#inbox-result').dataset.outcome === 'ok'`)
			assertDecisionRequest(t, f, path, body)
		})
	}
}

func TestBrowserRatificationButtonsValidateAndSendRedraft(t *testing.T) {
	f := newDecisionButtonFixture(t, strings.Replace(ratificationEntry("w_000"), `["ratify"]`, `[]`, 1))
	card := decisionSelector("ratification", "w_000", "")
	assertDecisionButtons(f.p, card, map[string]string{"redraft": "Ask for a Redraft"})
	f.p.click(card + `[data-decision-choice="redraft"]`)
	f.p.awaitText("#inbox-result", "Say what the redraft should change.")
	if len(f.got()) != 0 {
		t.Fatal("empty redraft note sent a request")
	}
	f.p.typeInto(card+`[name=note]`, "  Split the unit.  ")
	f.p.click(card + `[data-decision-choice="redraft"]`)
	f.p.awaitText("#inbox-result", "Decision recorded.")
	f.p.awaitVisible("the ratification card", card)
	assertDecisionRequest(t, f, "/v1/shed/redraft/w_000", map[string]any{"note": "Split the unit."})
}

func TestBrowserRatificationObjectionButtonsSendDispositions(t *testing.T) {
	for _, choice := range []string{"sustain", "overrule"} {
		t.Run(choice, func(t *testing.T) {
			f := newDecisionButtonFixture(t, ratificationEntry("w_000"))
			f.mu.Lock()
			f.packet = `{"packet":{"revision":{"spec":1,"plan":1},"dissent":[{"id":"objection-1","by":"owner"}]}}`
			f.mu.Unlock()
			f.api.setInbox(strings.Replace(ratificationEntry("w_000"), `"revision":1`, `"revision":2`, 1))
			f.api.pushEvent("inbox")
			card := decisionSelector("ratification", "w_000", "")
			f.p.awaitVisible("objection buttons", card+`[data-decision-choice="sustain objection-1"]`)
			assertDecisionButtons(f.p, card, map[string]string{"ratify": "Ratify", "redraft": "Ask for a Redraft", "sustain objection-1": "Sustain objection-1", "overrule objection-1": "Overrule objection-1"})
			f.p.typeInto(card+`[name=note]`, "Keep the contract.")
			f.p.click(card + `[data-decision-choice="` + choice + ` objection-1"]`)
			f.p.awaitText("#inbox-result", "Decision recorded.")
			f.p.awaitVisible("the ratification card", card)
			body := map[string]any{"objection": "objection-1"}
			path := "/v1/shed/overrule/w_000"
			if choice == "sustain" {
				path = "/v1/shed/rule/w_000"
				body["disposition"] = "sustain"
				body["note"] = "Keep the contract."
			} else {
				body["reason"] = "Keep the contract."
			}
			assertDecisionRequest(t, f, path, body)
		})
	}
}

func TestBrowserDecisionButtonsBlockDuringRequestAndKeepNoteOnFailure(t *testing.T) {
	f := newDecisionButtonFixture(t, ratificationEntry("w_000"))
	card := decisionSelector("ratification", "w_000", "")
	assertDecisionButtons(f.p, card, map[string]string{"ratify": "Ratify", "redraft": "Ask for a Redraft"})
	release := sync.OnceFunc(f.action.holdResponse())
	t.Cleanup(release)
	f.action.failNext("The packet changed.")
	f.p.typeInto(card+`[name=note]`, "Keep my note.")
	// Save references so the test can try every button after terminal dismissal.
	f.p.eval(`window.decisionButtons = [...document.querySelectorAll(`+quote(card+"[data-decision-choice]")+`)]`, nil)
	f.p.click(card + `[data-decision-choice="ratify"]`)
	f.p.awaitGone("the pending ratification", card)
	f.p.await("all card decisions blocked", `window.decisionButtons.every(b => b.disabled)`)
	f.p.eval(`window.decisionButtons.forEach(b => b.dispatchEvent(new MouseEvent('click')))`, nil)
	release()
	f.p.awaitText("#inbox-result", "The packet changed.")
	f.p.awaitVisible("the restored ratification", card)
	f.p.await("the note and available buttons", `document.querySelector(`+quote(card+`[name=note]`)+`).value === 'Keep my note.' && [...document.querySelectorAll(`+quote(card+"[data-decision-choice]")+`)].every(b => !b.disabled)`)
	assertDecisionRequest(t, f, "/v1/ratify/w_000", map[string]any{"spec": float64(1), "plan": float64(1)})
}

func TestBrowserParkedBaseWithoutOptionsHasNoVisibleForm(t *testing.T) {
	entry := `{"kind":"base","project":"p_fake","workstream":"w_000","options":[],"asked":[],"answer":{"method":"POST","path":"/v1/base/w_000/upstream","body":{}}}`
	f := newDecisionButtonFixture(t, entry)
	f.p.await("a parked base with no form", `document.querySelector('[data-kind=base] form').hidden && document.querySelectorAll('[data-kind=base] [data-decision-choice]').length === 0`)
	f.p.awaitText("[data-kind=base] [data-field=options]", "none until what blocks it is resolved")
}

func TestBrowserDecisionButtonsRefreshWhilePendingPreservesNoteAndBlocksNewChoices(t *testing.T) {
	entry := strings.Replace(ratificationEntry("w_000"), `["ratify"]`, `[]`, 1)
	f := newDecisionButtonFixture(t, entry)
	card := decisionSelector("ratification", "w_000", "")
	assertDecisionButtons(f.p, card, map[string]string{"redraft": "Ask for a Redraft"})
	release := sync.OnceFunc(f.action.holdResponse())
	t.Cleanup(release)
	f.action.failNext("Read the new packet.")
	f.p.typeInto(card+`[name=note]`, "Keep this draft.")
	f.p.click(card + `[data-decision-choice="redraft"]`)
	f.p.awaitText("#inbox-result", "Sending…")
	f.mu.Lock()
	f.packet = `{"packet":{"revision":{"spec":1,"plan":1},"dissent":[{"id":"new-objection","by":"owner"}]}}`
	f.mu.Unlock()
	f.api.setInbox(strings.Replace(ratificationEntry("w_000"), `"revision":1`, `"revision":2`, 1))
	f.api.pushEvent("inbox")
	f.p.awaitVisible("new objection buttons", card+`[data-decision-choice="sustain new-objection"]`)
	f.p.await("all refreshed choices blocked and note kept", `[...document.querySelectorAll(`+quote(card+"[data-decision-choice]")+`)].every(b => b.disabled) && document.querySelector(`+quote(card+`[name=note]`)+`).value === 'Keep this draft.'`)
	f.p.eval(`[...document.querySelectorAll(`+quote(card+"[data-decision-choice]")+`)].forEach(b => b.dispatchEvent(new MouseEvent('click')))`, nil)
	release()
	f.p.awaitText("#inbox-result", "Read the new packet.")
	f.p.await("all refreshed choices available and note kept", `[...document.querySelectorAll(`+quote(card+"[data-decision-choice]")+`)].every(b => !b.disabled) && document.querySelector(`+quote(card+`[name=note]`)+`).value === 'Keep this draft.'`)
	assertDecisionRequest(t, f, "/v1/shed/redraft/w_000", map[string]any{"note": "Keep this draft."})
}

// A creation that cannot finish waits on the project pane, from a start or
// not, and offers only a retry, which sends the creation's identity to the
// create route.
func TestBrowserStuckProjectCreationOffersOnlyRetry(t *testing.T) {
	const id = "p_0123456789abcdef0123456789abcdef"
	for _, workstream := range []string{"", "w_000"} {
		t.Run("workstream "+workstream, func(t *testing.T) {
			entry := fmt.Sprintf(`{"kind":"project-creation","project":"","workstream":%q,"number":0,"question":"Creating the project for owner/gadget stopped at step repository_created: cannot clone it. The host's git credential is the likely cause.","options":["retry"],"asked":[],"answer":{"method":"POST","path":"/v1/projects/create","body":{"id":%q}}}`, workstream, id)
			f := newDecisionButtonFixture(t, entry)
			f.p.click("#project-home")
			card := `[data-decision][data-kind="project-creation"] `
			f.p.awaitText(card+"[data-field=kind]", "Project creation stuck")
			f.p.awaitText(card+"[data-field=question]", "git credential")
			assertDecisionButtons(f.p, card, map[string]string{"retry": "Retry"})
			f.p.click(card + `[data-decision-choice="retry"]`)
			f.p.awaitGone("the retried card", card)
			f.p.await("the recorded outcome", `document.querySelector('#inbox-result').dataset.outcome === 'ok'`)
			assertDecisionRequest(t, f, "/v1/projects/create", map[string]any{"id": id})
		})
	}
}
