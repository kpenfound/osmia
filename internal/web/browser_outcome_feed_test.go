package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// outcomeFeedFixture serves the fake API plus the endpoints of the owner
// commands that report an outcome into the feed: a contested ruling, the
// workstream menu's drift rebase and recorded merge, and a charter rule
// decision.
type outcomeFeedFixture struct {
	api *fakeAPI
	p   *page
	// rebase, merged and charter answer the menu's and the proposals'
	// commands; rulings counts the contested rulings answered.
	rebase, merged, charter fakeAction
	rulings                 atomic.Int32
}

// transitionEntry is the JSON of a feed entry for a workstream transition.
func transitionEntry(at, to string) string {
	return fmt.Sprintf(`{"kind":"transition","at":%s,"transition":{"subject":"feature","from":"planning","to":%s,"actor":"owner","reason":""}}`,
		quote(at), quote(to))
}

// charterProposals is the JSON of a charter read with one proposal on
// workstream.
func charterProposals(workstream string) string {
	return fmt.Sprintf(`{"proposals":[{"workstream":%s,"question":"q1","rule":"Keep changes small.","owner_response":"Agreed."}]}`, quote(workstream))
}

// newOutcomeFeedFixture opens the page on a fake API with workstreams
// workstreams, w_000 selected, and lets configure set up the API before the
// page loads.
func newOutcomeFeedFixture(t *testing.T, workstreams int, configure func(*fakeAPI)) *outcomeFeedFixture {
	t.Helper()
	f := &outcomeFeedFixture{api: newFakeAPI(workstreams)}
	configure(f.api)
	base := f.api.handler()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/contested/", func(w http.ResponseWriter, r *http.Request) {
		f.api.contested.serve(w, r, `{}`)
		f.rulings.Add(1)
	})
	mux.HandleFunc("/v1/rebase/", func(w http.ResponseWriter, r *http.Request) {
		f.rebase.serve(w, r, `{"drift":3}`)
	})
	mux.HandleFunc("/v1/delivery/", func(w http.ResponseWriter, r *http.Request) {
		f.merged.serve(w, r, `{"merge":{"upstream":{"remote":"origin","branch":"main"},"branch":"osmia/w_000"}}`)
	})
	mux.HandleFunc("/v1/charter/", func(w http.ResponseWriter, r *http.Request) {
		f.charter.serve(w, r, `{}`)
	})
	// The subtree patterns above would redirect a read of /v1/charter.
	mux.Handle("/v1/charter", base)
	mux.Handle("/", base)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	f.p = openBrowser(t)
	f.p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	f.p.await("the live connection", `document.body.dataset.connection === 'live'`)
	f.p.click(`[data-select="w_000"]`)
	return f
}

// feedKinds lists the kind of each entry of the displayed workstream's feed,
// in order.
func (p *page) feedKinds() string {
	p.t.Helper()
	var kinds string
	p.eval(`[...document.querySelectorAll('#workstreams [data-field=feed] ol > li')].map(n => n.dataset.kind).join(',')`, &kinds)
	return kinds
}

// awaitFeedKinds waits until the displayed workstream's feed shows entries of
// exactly these kinds, in order.
func (p *page) awaitFeedKinds(want string) {
	p.t.Helper()
	p.await("the feed entries "+want, `[...document.querySelectorAll('#workstreams [data-field=feed] ol > li')].map(n => n.dataset.kind).join(',') === `+quote(want))
}

// awaitNoUnread waits until no sidebar row is marked as having new activity.
func (p *page) awaitNoUnread() {
	p.t.Helper()
	p.await("no unread mark in the sidebar", `document.querySelector('#sidebar [data-field=unread]') === null`)
}

// settle lets the response continuation and the page's paint both finish.
func (p *page) settle() {
	p.t.Helper()
	p.run(chromedp.Evaluate(`new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))`, nil,
		func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams { return p.WithAwaitPromise(true) }))
}

// The outcome of an accepted ruling becomes an entry of the feed, after the
// entries the page showed when the command was accepted. Entries the service
// adds later follow it, and reading the feed again, with an inbox event and a
// full resync, leaves exactly one outcome entry without marking anything
// unread.
func TestBrowserAcceptedOutcomeIsFeedEntryBeforeLaterEntries(t *testing.T) {
	f := newOutcomeFeedFixture(t, 2, func(api *fakeAPI) {
		api.setInbox(contestedEntry("w_000", "index"))
		api.setFeed("w_000", transitionEntry("2026-01-01T00:00:01Z", "building"))
	})
	p := f.p
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)
	p.awaitFeedKinds("transition")
	p.awaitNoUnread()

	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + `[data-decision-choice="review"]`)
	p.awaitOutcome("Ruled review on unit index.")

	// The entry says whose command it is and when it was accepted, follows
	// the entry shown when the command was accepted, and leaves the shared
	// result line empty.
	p.awaitFeedKinds("transition,outcome")
	p.assertOutcomes("#workstreams", "Ruled review on unit index.")
	var marked bool
	p.eval(`(() => {
		const entry = document.querySelector('#workstreams [data-kind=outcome]');
		return entry.dataset.outcome === 'ok' && entry.querySelector('[data-ann=role]').textContent === 'Your command' &&
			entry.querySelector('[data-ann=when]').textContent !== '' && document.querySelector('#inbox-result').dataset.outcome === '';
	})()`, &marked)
	if !marked {
		t.Fatal("the outcome entry is not marked as the owner's command outcome with its time")
	}

	// The service adds an entry afterwards: it renders after the outcome,
	// even though the feed orders it by a time earlier than the entry shown
	// when the command was accepted, as a service clock behind the page's does.
	api := f.api
	api.setFeed("w_000", transitionEntry("2025-12-31T00:00:09Z", "assembled"), transitionEntry("2026-01-01T00:00:01Z", "building"))
	api.pushEvent("resync")
	p.awaitFeedKinds("transition,outcome,transition")
	var order string
	p.eval(`[...document.querySelectorAll('#workstreams [data-field=feed] ol > li')].map(n => n.dataset.kind === 'outcome' ? 'outcome' : n.textContent.includes('assembled') ? 'assembled' : 'building').join(',')`, &order)
	if order != "building,outcome,assembled" {
		t.Fatalf("feed order = %q, want the entry the service added after the outcome", order)
	}

	// Further reads of the inbox and the feed do not duplicate the outcome
	// or mark the workstream unread.
	hits := api.inboxRequests()
	api.pushEvent("inbox")
	soon(t, "the page never read the inbox again", func() bool { return api.inboxRequests() > hits })
	api.pushEvent("resync")
	p.awaitFeedKinds("transition,outcome,transition")
	p.assertOutcomes("#workstreams", "Ruled review on unit index.")
	p.awaitNoUnread()
	p.click(`[data-select="w_001"]`)
	p.click(`[data-select="w_000"]`)
	p.awaitFeedKinds("transition,outcome,transition")
	p.awaitNoUnread()
}

// A refused command leaves no feed entry: its reason stays in the shared
// result line beside the restored card. A later accepted command adds one.
func TestBrowserRefusedCommandAddsNoFeedEntry(t *testing.T) {
	f := newOutcomeFeedFixture(t, 1, func(api *fakeAPI) {
		api.setInbox(contestedEntry("w_000", "index"))
	})
	p := f.p
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)
	f.api.contested.failNext("the reviewer's turn is still running")
	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + `[data-decision-choice="review"]`)
	p.awaitText("#inbox-result", "the reviewer's turn is still running")
	if outcome := p.outcomeOf("#inbox-result"); outcome != "error" {
		t.Fatalf("rejected outcome: data-outcome=%q", outcome)
	}
	p.awaitVisible("the contested unit, restored", card)
	p.assertNoOutcomes("#workstreams")

	f.api.contested.failNextUnavailable("sealing workstream w_000 is already in progress")
	p.click(card + `[data-decision-choice="review"]`)
	p.awaitText("#inbox-result", "sealing workstream w_000 is already in progress")
	if outcome := p.outcomeOf("#inbox-result"); outcome != "unavailable" {
		t.Fatalf("unavailable outcome: data-outcome=%q", outcome)
	}
	p.awaitVisible("the contested unit, restored", card)
	p.assertNoOutcomes("#workstreams")

	f.api.contested.failNext("")
	p.click(card + `[data-decision-choice="review"]`)
	p.awaitOutcome("Ruled review on unit index.")
	p.assertOutcomes("#workstreams", "Ruled review on unit index.")
}

// The workstream menu's drift rebase and recorded merge, taken on a
// workstream, put their accepted outcomes into its feed in the order they
// were accepted, and nothing into the shared result line.
func TestBrowserMenuActionOutcomesAreFeedEntries(t *testing.T) {
	f := newOutcomeFeedFixture(t, 1, func(api *fakeAPI) { api.state = "assembled" })
	p := f.p
	p.click("#workstream-menu-button")
	p.await("the menu actions offered", `!document.querySelector('[data-workstream-action="rebase"]').hidden && !document.querySelector('[data-workstream-action="merged"]').hidden`)
	p.click(`[data-workstream-action="rebase"]`)
	p.awaitOutcome("Drift rebase 3 of Workstream 0 is requested")
	p.assertOutcomes("#workstreams", "Drift rebase 3 of Workstream 0 is requested; the foreman runs it once no landing or other drift rebase holds the project.")

	p.click("#workstream-menu-button")
	p.click(`[data-workstream-action="merged"]`)
	p.awaitOutcome("Workstream 0 is delivered: origin/main holds osmia/w_000.")
	p.assertOutcomes("#workstreams",
		"Drift rebase 3 of Workstream 0 is requested; the foreman runs it once no landing or other drift rebase holds the project.",
		"Workstream 0 is delivered: origin/main holds osmia/w_000.")
}

// A refused menu action keeps its reason in the shared result line and adds
// no feed entry.
func TestBrowserRefusedMenuActionAddsNoFeedEntry(t *testing.T) {
	f := newOutcomeFeedFixture(t, 1, func(api *fakeAPI) {})
	p := f.p
	f.rebase.failNext("a drift rebase is already requested")
	p.click("#workstream-menu-button")
	p.click(`[data-workstream-action="rebase"]`)
	p.awaitText("#inbox-result", "a drift rebase is already requested")
	if outcome := p.outcomeOf("#inbox-result"); outcome != "error" {
		t.Fatalf("rejected outcome: data-outcome=%q", outcome)
	}
	p.assertNoOutcomes("#workstreams")
}

// A charter rule decision puts its accepted outcome into the feed of the
// workstream the proposal belongs to.
func TestBrowserCharterRuleDecisionOutcomeIsFeedEntry(t *testing.T) {
	f := newOutcomeFeedFixture(t, 1, func(api *fakeAPI) { api.charter = charterProposals("w_000") })
	p := f.p
	proposal := `[data-proposal="w_000:q1"] `
	p.awaitVisible("the charter proposal", proposal)
	p.click(proposal + `[data-field=ratify]`)
	p.awaitOutcome("Charter decision recorded.")
	p.assertOutcomes("#workstreams", "Charter decision recorded.")
}

// A response that arrives after the owner opened another workstream puts its
// outcome only into the feed of the workstream the command was taken on,
// where it is when the owner returns, and shows nothing in the other one.
func TestBrowserLateOutcomeBelongsToTheOriginatingFeed(t *testing.T) {
	f := newOutcomeFeedFixture(t, 2, func(api *fakeAPI) {
		api.setInbox(contestedEntry("w_000", "index"))
		api.setFeed("w_000", transitionEntry("2026-01-01T00:00:01Z", "building"))
		api.setFeed("w_001", transitionEntry("2026-01-01T00:00:02Z", "building"))
	})
	p := f.p
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)
	release := f.api.contested.holdResponse()
	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + `[data-decision-choice="review"]`)
	p.awaitText("#inbox-result", "Sending…")

	p.click(`[data-select="w_001"]`)
	p.await("the other workstream shown", `document.getElementById('workstream-head').dataset.workstream === 'w_001'`)
	p.assertNoOutcomes("#workstreams")
	release()
	soon(t, "the held ruling was never answered", func() bool { return f.rulings.Load() == 1 })
	p.settle()

	// The other workstream shows no outcome entry and no result.
	p.awaitFeedKinds("transition")
	p.assertOutcomes("#workstreams")
	if outcome := p.outcomeOf("#inbox-result"); outcome != "" {
		t.Fatalf("the other workstream's result line shows data-outcome=%q", outcome)
	}
	p.awaitNoUnread()

	// Returning shows the outcome, once, in the originating feed.
	p.click(`[data-select="w_000"]`)
	p.awaitOutcome("Ruled review on unit index.")
	p.awaitFeedKinds("transition,outcome")
	p.assertOutcomes("#workstreams", "Ruled review on unit index.")
	p.click(`[data-select="w_001"]`)
	p.awaitFeedKinds("transition")
	p.assertOutcomes("#workstreams")
	if strings.Contains(p.feedKinds(), "outcome") {
		t.Fatal("the other workstream's feed shows the outcome")
	}
}
