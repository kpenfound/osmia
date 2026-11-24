package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

type outcomeFeedFixture struct {
	api                     *fakeAPI
	p                       *page
	rebase, merged, charter fakeAction
	rulings                 counter
}

func transitionEntry(at, to string) string {
	return fmt.Sprintf(`{"kind":"transition","at":%s,"transition":{"subject":"feature","from":"planning","to":%s,"actor":"owner","reason":""}}`,
		quote(at), quote(to))
}

func charterProposals(workstream string) string {
	return fmt.Sprintf(`{"proposals":[{"workstream":%s,"question":"q1","rule":"Keep changes small.","owner_response":"Agreed."}]}`, quote(workstream))
}

func newOutcomeFeedFixture(t *testing.T, workstreams int, configure func(*fakeAPI)) *outcomeFeedFixture {
	t.Helper()
	f := &outcomeFeedFixture{api: newFakeAPI(workstreams)}
	configure(f.api)
	base := f.api.handler()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/contested/", func(w http.ResponseWriter, r *http.Request) {
		f.api.contested.serve(w, r, `{}`)
		f.rulings.add()
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

func (p *page) feedKinds() string {
	p.t.Helper()
	var kinds string
	p.eval(`[...document.querySelectorAll('#workstreams [data-field=feed] ol > li')].map(n => n.dataset.kind).join(',')`, &kinds)
	return kinds
}

func (p *page) awaitFeedKinds(want string) {
	p.t.Helper()
	p.await("the feed entries "+want, `[...document.querySelectorAll('#workstreams [data-field=feed] ol > li')].map(n => n.dataset.kind).join(',') === `+quote(want))
}

func (p *page) awaitNoUnread() {
	p.t.Helper()
	p.await("no unread mark in the sidebar", `document.querySelector('#sidebar [data-field=unread]') === null`)
}

func (p *page) settle() {
	p.t.Helper()
	p.run(chromedp.Evaluate(`new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))`, nil,
		func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams { return p.WithAwaitPromise(true) }))
}

func TestBrowserAcceptedOutcomeIsFeedEntryBeforeLaterEntries(t *testing.T) {
	t.Parallel()
	f := newOutcomeFeedFixture(t, 2, func(api *fakeAPI) {
		api.setInbox(contestedEntry("w_000", "index"))
		api.setFeed("w_000", transitionEntry("2026-01-01T00:00:01Z", "building"))
	})
	p := f.p
	card := decisionSelector("contested", "w_000", "index")
	p.awaitVisible("the contested unit", card)
	p.awaitFeedKinds("milestone")
	p.awaitNoUnread()

	p.typeInto(card+`input[name=note]`, "Review it again.")
	p.click(card + `[data-decision-choice="review"]`)
	p.awaitOutcome("Ruled review on unit index.")

	p.awaitFeedKinds("milestone,outcome")
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

	api := f.api
	api.setFeed("w_000", transitionEntry("2025-12-31T00:00:09Z", "assembled"), transitionEntry("2026-01-01T00:00:01Z", "building"))
	api.pushEvent("resync")
	p.awaitFeedKinds("milestone,outcome,milestone")
	var order string
	p.eval(`[...document.querySelectorAll('#workstreams [data-field=feed] ol > li')].map(n => n.dataset.kind === 'outcome' ? 'outcome' : n.textContent.includes('assembled') ? 'assembled' : 'building').join(',')`, &order)
	if order != "building,outcome,assembled" {
		t.Fatalf("feed order = %q, want the entry the service added after the outcome", order)
	}

	hits := api.inboxRequests()
	api.pushEvent("inbox")
	api.inbox.await(t, "the page never read the inbox again", func(n int) bool { return n > hits })
	api.pushEvent("resync")
	p.awaitFeedKinds("milestone,outcome,milestone")
	p.assertOutcomes("#workstreams", "Ruled review on unit index.")
	p.awaitNoUnread()
	p.click(`[data-select="w_001"]`)
	p.click(`[data-select="w_000"]`)
	p.awaitFeedKinds("milestone,outcome,milestone")
	p.awaitNoUnread()
}

func TestBrowserRefusedCommandAddsNoFeedEntry(t *testing.T) {
	t.Parallel()
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

func TestBrowserMenuActionOutcomesAreFeedEntries(t *testing.T) {
	t.Parallel()
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

func TestBrowserRefusedMenuActionAddsNoFeedEntry(t *testing.T) {
	t.Parallel()
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

func TestBrowserCharterRuleDecisionOutcomeIsFeedEntry(t *testing.T) {
	t.Parallel()
	f := newOutcomeFeedFixture(t, 1, func(api *fakeAPI) { api.charter = charterProposals("w_000") })
	p := f.p
	proposal := `[data-proposal="w_000:q1"] `
	p.awaitVisible("the charter proposal", proposal)
	p.click(proposal + `[data-field=ratify]`)
	p.awaitOutcome("Charter decision recorded.")
	p.assertOutcomes("#workstreams", "Charter decision recorded.")
}

func TestBrowserLateOutcomeBelongsToTheOriginatingFeed(t *testing.T) {
	t.Parallel()
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
	f.rulings.await(t, "the held ruling was never answered", func(n int) bool { return n == 1 })
	p.settle()

	p.awaitFeedKinds("milestone")
	p.assertOutcomes("#workstreams")
	if outcome := p.outcomeOf("#inbox-result"); outcome != "" {
		t.Fatalf("the other workstream's result line shows data-outcome=%q", outcome)
	}
	p.awaitNoUnread()

	p.click(`[data-select="w_000"]`)
	p.awaitOutcome("Ruled review on unit index.")
	p.awaitFeedKinds("milestone,outcome")
	p.assertOutcomes("#workstreams", "Ruled review on unit index.")
	p.click(`[data-select="w_001"]`)
	p.awaitFeedKinds("milestone")
	p.assertOutcomes("#workstreams")
	if strings.Contains(p.feedKinds(), "outcome") {
		t.Fatal("the other workstream's feed shows the outcome")
	}
}
