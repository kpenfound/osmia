package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func feedAt(n int) string {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute).Format(time.RFC3339)
}

func chatEntry(kind string, n int, text string) string {
	return fmt.Sprintf(`{"kind":%s,"at":%s,"turn":"t%d","text":%s,"state":"done"}`, jsonString(kind), jsonString(feedAt(n)), n, jsonString(text))
}

func statusEntry(n int, note, attention string) string {
	return fmt.Sprintf(`{"kind":"status","at":%s,"status":{"goal":"Ship the uploads","attention":%s,"note":%s,"agents":[],"revision":%d,"updated_at":%s}}`,
		jsonString(feedAt(n)), jsonString(attention), jsonString(note), n, jsonString(feedAt(n)))
}

func sessionEntry(n int, unit, state, report string) string {
	failure, summary := "", report
	if state == "failed" {
		failure, summary = report, ""
	}
	return fmt.Sprintf(`{"kind":"session","at":%s,"session":{"agent":"mason-%s","role":"mason","unit":%s,"turn":"s%d","profile":"default","state":%s,"started_at":%s,"ended_at":%s,"summary":%s,"failure":%s}}`,
		jsonString(feedAt(n)), unit, jsonString(unit), n, jsonString(state), jsonString(feedAt(n)), jsonString(feedAt(n+1)), jsonString(summary), jsonString(failure))
}

func unitTransitionEntry(n int, unit, from, to, reason string) string {
	return fmt.Sprintf(`{"kind":"transition","at":%s,"transition":{"subject":"unit-%s","unit":%s,"from":%s,"to":%s,"actor":"service:scheduler","reason":%s}}`,
		jsonString(feedAt(n)), unit, jsonString(unit), jsonString(from), jsonString(to), jsonString(reason))
}

func featureTransitionEntry(n int, to, reason string) string {
	return fmt.Sprintf(`{"kind":"transition","at":%s,"transition":{"subject":"feature","from":"planning","to":%s,"actor":"owner:local","reason":%s}}`,
		jsonString(feedAt(n)), jsonString(to), jsonString(reason))
}

func unitChecksEntry(n int, unit string, run int, state string) string {
	failed := ""
	if state == "failed" {
		failed = `"failed":["dag://go/test"],`
	}
	return fmt.Sprintf(`{"kind":"unit_checks","at":%s,"unit_checks":{"unit":%s,"run":%d,"state":%s,%s"started_at":%s}}`,
		jsonString(feedAt(n)), jsonString(unit), run, jsonString(state), failed, jsonString(feedAt(n)))
}

func finalReviewEntry(n, review int, state string) string {
	failure := ""
	if state == "failed" {
		failure = `,"failure":"the reader did not produce a report"`
	}
	return fmt.Sprintf(`{"kind":"final_review","at":%s,"final_review":{"review":%d,"state":%s,"reader":"reader","turn":"s%d","blocking":1,"gaps":2%s}}`,
		jsonString(feedAt(n)), review, jsonString(state), n, failure)
}

type feedPage struct {
	*page
	url string
}

func openFeedPage(t *testing.T, workstreams int, hash string, feeds map[string][]string) *feedPage {
	t.Helper()
	api := newFakeAPI(workstreams)
	for id, entries := range feeds {
		api.setFeed(id, entries...)
	}
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"+hash))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	if hash == "" {
		p.click(`[data-select="w_000"]`)
	}
	p.await("the workstream shown", `document.getElementById('workstream-head').dataset.workstream === 'w_000'`)
	return &feedPage{page: p, url: server.URL}
}

func conversationFeed() []string {
	return []string{
		chatEntry("message", 1, "Start the upload work."),
		chatEntry("response", 2, "Understood, I will plan it."),
		statusEntry(3, "Planning the uploads.", ""),
		featureTransitionEntry(4, "ratified", "the owner ratified revision 1"),
		featureTransitionEntry(5, "building", "sealed and building"),
		unitTransitionEntry(6, "upload", "ready", "implementing", "a mason started"),
		sessionEntry(7, "upload", "failed", "the sandbox could not start"),
		unitChecksEntry(9, "upload", 1, "failed"),
		sessionEntry(10, "upload", "done", "Built the upload unit."),
		unitTransitionEntry(12, "upload", "implementing", "merged", "checks passed"),
		sessionEntry(13, "dedupe", "done", "Built the dedupe unit."),
		unitChecksEntry(15, "dedupe", 1, "passed"),
		statusEntry(16, "Both units are built.", ""),
		featureTransitionEntry(17, "assembled", "every unit merged"),
		finalReviewEntry(18, 1, "reviewed"),
		chatEntry("action", 19, "Moved the workstream to final review."),
		statusEntry(20, "Waiting for the final review.", "Approve delivery."),
		featureTransitionEntry(21, "delivered", "the owner approved delivery"),
		sessionEntry(22, "dedupe", "done", "Cleaned up."),
		unitChecksEntry(24, "dedupe", 2, "passed"),
		unitTransitionEntry(25, "dedupe", "approved", "merged", "checks passed"),
	}
}

func openConversationPage(t *testing.T) *page {
	t.Helper()
	return openFeedPage(t, 1, "", map[string][]string{"w_000": conversationFeed()}).page
}

type conversationShown struct{ Milestones, Statuses, Latest, Machine string }

func readConversationShown(p *page) conversationShown {
	var shown conversationShown
	p.eval(`({
  milestones: [...document.querySelectorAll('#workstreams [data-kind=milestone]')].map(n => n.dataset.milestone).join(','),
  statuses: String(document.querySelectorAll('#workstreams [data-kind=status]').length),
  latest: document.querySelector('#workstreams [data-kind=status] [data-field=note]').textContent,
  machine: String(document.querySelectorAll('#workstreams [data-kind=session], #workstreams [data-kind=transition], #workstreams [data-kind=unit_checks], #workstreams [data-kind=final_checks], #workstreams [data-kind=session_checks], #workstreams [data-kind=final_review]').length),
 })`, &shown)
	return shown
}

func TestBrowserTabsAreConversationActivityDocumentsAndTrace(t *testing.T) {
	t.Parallel()
	p := openConversationPage(t)
	var tabs []string
	p.eval(`[...document.querySelectorAll('[role=tab]')].map(t => t.textContent)`, &tabs)
	if strings.Join(tabs, ",") != "Conversation,Activity,Documents,Trace" {
		t.Fatalf("the tabs are %q", tabs)
	}
}

func TestBrowserConversationIsTheDefaultTab(t *testing.T) {
	t.Parallel()
	p := openConversationPage(t)
	p.await("Conversation selected", `document.querySelector('[data-tab=conversation]').getAttribute('aria-selected') === 'true' && !document.querySelector('[data-panel=conversation]').hidden && document.querySelector('[data-panel=activity]').hidden`)
}

func TestBrowserConversationHoldsMessagesRepliesMilestonesActionsAndOneStatus(t *testing.T) {
	t.Parallel()
	p := openConversationPage(t)
	p.awaitFeedKinds("message,response,milestone,milestone,milestone,milestone,action,status,milestone")
	if shown := readConversationShown(p); shown.Statuses != "1" || shown.Latest != "Waiting for the final review." {
		t.Fatalf("Conversation shows %s statuses, the latest %q", shown.Statuses, shown.Latest)
	}
}

func TestBrowserConversationShowsTheMilestonesInOrder(t *testing.T) {
	t.Parallel()
	p := openConversationPage(t)
	p.awaitFeedKinds("message,response,milestone,milestone,milestone,milestone,action,status,milestone")
	if shown := readConversationShown(p); shown.Milestones != "ratified,building,assembled,final_review,delivered" {
		t.Fatalf("milestones = %q", shown.Milestones)
	}
}

func TestBrowserConversationShowsNoMachineEvents(t *testing.T) {
	t.Parallel()
	p := openConversationPage(t)
	p.awaitFeedKinds("message,response,milestone,milestone,milestone,milestone,action,status,milestone")
	if shown := readConversationShown(p); shown.Machine != "0" {
		t.Fatalf("Conversation shows %s machine events", shown.Machine)
	}
}

func TestBrowserConversationMilestonesDescribeTheirEvents(t *testing.T) {
	t.Parallel()
	p := openConversationPage(t)
	p.awaitText(`#workstreams [data-milestone=final_review]`, "Final review 1")
	p.awaitText(`#workstreams [data-milestone=final_review]`, "1 blocking findings, 2 uncovered criteria")
	p.awaitText(`#workstreams [data-milestone=delivered]`, "Workstream")
}

func TestBrowserConversationNotesFinalReviewFailuresButNotFailedChecksOrSessions(t *testing.T) {
	t.Parallel()
	f := openFeedPage(t, 1, "", map[string][]string{"w_000": {
		featureTransitionEntry(1, "building", "building"),
		sessionEntry(2, "upload", "failed", "the sandbox could not start"),
		unitChecksEntry(4, "upload", 1, "failed"),
		unitTransitionEntry(5, "upload", "implementing", "contested", "checks failed three times"),
		finalReviewEntry(6, 1, "failed"),
	}})
	f.awaitFeedKinds("milestone,milestone")
	f.awaitText(`#workstreams [data-milestone=final_review]`, "the reader did not produce a report")
}

func TestBrowserMilestoneOpensActivityAtItsEntry(t *testing.T) {
	t.Parallel()
	entries := []string{featureTransitionEntry(1, "building", "building"), finalReviewEntry(2, 1, "reviewed")}
	for n := 0; n < 60; n++ {
		entries = append(entries, statusEntry(10+n, fmt.Sprintf("Status number %d.", n), ""))
	}
	entries = append(entries, chatEntry("message", 100, "Where is the review?"))
	f := openFeedPage(t, 1, "", map[string][]string{"w_000": entries})
	p := f.page
	p.awaitText(`#workstreams [data-milestone=final_review]`, "Final review 1")
	p.click(`#workstreams [data-milestone=final_review] a`)
	p.await("Activity open at the review", `!document.querySelector('[data-panel=activity]').hidden && document.querySelector('[data-tab=activity]').getAttribute('aria-selected') === 'true'`)
	p.await("the review entry highlighted and in view", `(() => {
  const entry = document.querySelector('#workstream-activity [data-kind=final_review]');
  if (!entry || entry.dataset.focus !== 'true') return false;
  const main = document.getElementById('main').getBoundingClientRect();
  const box = entry.getBoundingClientRect();
  return box.top >= main.top - 1 && box.bottom <= main.bottom + 1;
 })()`)
	var hash string
	p.eval(`location.hash`, &hash)
	if !strings.HasPrefix(hash, "#workstream=w_000&tab=activity&entry=") {
		t.Fatalf("the location is %q", hash)
	}
	p.eval(`history.back()`, nil)
	p.await("Conversation again", `!document.querySelector('[data-panel=conversation]').hidden && location.hash === '#workstream=w_000'`)
	p.eval(`history.forward()`, nil)
	p.await("Activity at the entry again", `!document.querySelector('[data-panel=activity]').hidden && document.querySelector('#workstream-activity [data-kind=final_review]')?.dataset.focus === 'true'`)
}

func openActivityPage(t *testing.T) *page {
	t.Helper()
	f := openFeedPage(t, 1, "", map[string][]string{"w_000": conversationFeed()})
	p := f.page
	p.eval(`window.sameDocument = true`, nil)
	p.click(`[data-tab=activity]`)
	p.await("Activity shown", `!document.querySelector('[data-panel=activity]').hidden && document.querySelector('#workstream-activity .activity-log > ol')`)
	return p
}

func openEveryActivityFold(p *page) {
	p.eval(`document.querySelectorAll('#workstream-activity .fold:not([open])').forEach(f => { f.open = true; })`, nil)
}

func TestBrowserActivityFoldsRunsOfEventsIntoClosedGroups(t *testing.T) {
	t.Parallel()
	p := openActivityPage(t)
	var folds []struct {
		Count string
		Open  bool
		Text  string
	}
	p.eval(`[...document.querySelectorAll('#workstream-activity .fold')].map(f => ({count: f.querySelector('[data-field=count]').textContent, open: f.open, text: f.querySelector('summary').textContent}))`, &folds)
	if len(folds) != 2 || folds[0].Count != "7 events" || folds[1].Count != "3 events" || folds[0].Open || folds[1].Open {
		t.Fatalf("folds = %+v, want a closed run of 7 events and a closed run of 3", folds)
	}
	if !strings.Contains(folds[0].Text, "3 sessions, 2 state changes, 2 check runs") || !strings.Contains(folds[1].Text, "1 session, 1 state change, 1 check run") {
		t.Fatalf("the folds read %q and %q", folds[0].Text, folds[1].Text)
	}
}

func TestBrowserActivityHidesFoldedEventsUntilTheFoldOpens(t *testing.T) {
	t.Parallel()
	p := openActivityPage(t)
	p.await("the unit session hidden inside its fold", `(() => { const s = document.querySelector('#workstream-activity [data-kind=session][data-turn=s7]'); return s && !s.checkVisibility() && !s.closest('.fold').open; })()`)
}

func TestBrowserActivityOpensAFoldInPlace(t *testing.T) {
	t.Parallel()
	p := openActivityPage(t)
	p.click(`#workstream-activity .fold summary`)
	p.await("the fold open in place", `(() => {
  const s = document.querySelector('#workstream-activity [data-kind=session][data-turn=s7]');
  return window.sameDocument === true && s.checkVisibility() && document.querySelector('#workstream-activity .fold').open;
 })()`)
}

func TestBrowserActivityShowsEveryEventKind(t *testing.T) {
	t.Parallel()
	p := openActivityPage(t)
	openEveryActivityFold(p)
	p.await("every event kind", `(() => {
  const has = (selector) => document.querySelector('#workstream-activity ' + selector) !== null;
  return has('[data-kind=session][data-state=failed]') && has('[data-kind=session][data-state=done]') && has('[data-kind=unit_checks][data-state=failed]') &&
   has('[data-kind=unit_checks][data-state=passed]') && has('[data-kind=final_review][data-state=reviewed]') && has('[data-kind=status][data-latest=true]') &&
   document.querySelectorAll('#workstream-activity [data-kind=status]').length === 3 && document.querySelectorAll('#workstream-activity [data-kind=transition]').length === 7;
 })()`)
}

func TestBrowserActivityEventsCarryTheirDetails(t *testing.T) {
	t.Parallel()
	p := openActivityPage(t)
	openEveryActivityFold(p)
	p.awaitText(`#workstream-activity [data-kind=transition][data-to=merged] [data-field=reason]`, "checks passed")
	p.awaitText(`#workstream-activity [data-kind=transition][data-to=ratified] [data-field=reason]`, "the owner ratified revision 1")
	p.awaitText(`#workstream-activity [data-kind=session][data-state=failed]`, "the sandbox could not start")
	p.awaitText(`#workstream-activity [data-kind=unit_checks][data-state=failed] [data-field=failed]`, "dag://go/test")
	p.awaitText(`#workstream-activity [data-kind=final_review] [data-field=findings]`, "1 blocking findings")
	p.awaitText(`#workstream-activity [data-kind=session][data-state=done] [data-field=summary]`, "Built the upload unit.")
}

func TestBrowserActivityOffersOnlyAUnitFilter(t *testing.T) {
	t.Parallel()
	p := openActivityPage(t)
	var controls []string
	p.eval(`[...document.querySelectorAll('#workstream-activity .activity-bar select, #workstream-activity .activity-bar input, #workstream-activity .activity-bar button')].map(c => c.tagName.toLowerCase() + ':' + (c.getAttribute('aria-label') || c.name))`, &controls)
	if strings.Join(controls, ",") != "select:Filter by unit" {
		t.Fatalf("the Activity controls are %q", controls)
	}
}

func TestBrowserActivityUnitFilterListsTheUnitsOfTheEvents(t *testing.T) {
	t.Parallel()
	p := openActivityPage(t)
	var units []string
	p.eval(`[...document.querySelectorAll('#workstream-activity select[name=unit] option')].map(o => o.value)`, &units)
	if strings.Join(units, ",") != ",dedupe,upload" {
		t.Fatalf("the unit filter offers %q", units)
	}
}

func TestBrowserActivityUnitFilterNarrowsTheEventsWithoutReloading(t *testing.T) {
	t.Parallel()
	p := openActivityPage(t)
	openEveryActivityFold(p)
	p.eval(`(() => { const s = document.querySelector('#workstream-activity select[name=unit]'); s.value = 'dedupe'; s.dispatchEvent(new Event('change', {bubbles: true})); })()`, nil)
	p.await("only the dedupe unit's events", `(() => {
  const entries = [...document.querySelectorAll('#workstream-activity [data-entry]')];
  return entries.length === 5 && !document.querySelector('#workstream-activity [data-turn=s7], #workstream-activity [data-turn=s10], #workstream-activity [data-unit=upload]') &&
   location.hash.includes('unit=dedupe');
 })()`)
	p.await("the same document", `window.sameDocument === true`)
}

func openActivityLocation(t *testing.T) (f *feedPage, hash string) {
	t.Helper()
	f = openFeedPage(t, 1, "", map[string][]string{"w_000": conversationFeed()})
	p := f.page
	p.click(`[data-tab=activity]`)
	p.await("Activity shown", `document.querySelector('#workstream-activity .activity-log > ol')`)
	p.eval(`(() => { const s = document.querySelector('#workstream-activity select[name=unit]'); s.value = 'upload'; s.dispatchEvent(new Event('change', {bubbles: true})); })()`, nil)
	p.await("the upload unit's events", `location.hash.includes('unit=upload') && !document.querySelector('#workstream-activity [data-turn=s13]')`)
	p.click(`#workstream-activity .fold summary`)
	p.await("the fold open", `document.querySelector('#workstream-activity .fold').open`)
	p.click(`#workstream-activity [data-kind=session][data-turn=s10] summary`)
	p.await("the report open", `document.querySelector('#workstream-activity [data-kind=session][data-turn=s10] details').open && location.hash.includes('turn-s10')`)
	p.eval(`location.hash`, &hash)
	return f, hash
}

func TestBrowserActivityLocationNamesTheTabFilterAndOpenedEntry(t *testing.T) {
	t.Parallel()
	_, hash := openActivityLocation(t)
	if !strings.HasPrefix(hash, "#workstream=w_000&tab=activity&unit=upload&open=") {
		t.Fatalf("the location is %q", hash)
	}
}

func openRestoredActivity(t *testing.T) (fresh *page, hash string) {
	t.Helper()
	f, hash := openActivityLocation(t)
	fresh = openBrowser(t)
	fresh.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(f.url+"/"+hash))
	fresh.await("the live connection", `document.body.dataset.connection === 'live'`)
	fresh.await("Activity drawn", `document.querySelector('#workstream-activity [data-kind=session][data-turn=s10]')`)
	return fresh, hash
}

func TestBrowserActivityLocationRestoresTheTabFilterAndOpenedEntry(t *testing.T) {
	t.Parallel()
	fresh, _ := openRestoredActivity(t)
	var view struct {
		Panel, Tab, Filter, Details, Open, Visible string
	}
	fresh.eval(`(() => {
  const s = document.querySelector('#workstream-activity [data-kind=session][data-turn=s10] details');
  return {
   panel: String(!document.querySelector('[data-panel=activity]').hidden), tab: document.querySelector('[data-tab=activity]').getAttribute('aria-selected'),
   filter: document.querySelector('#workstream-activity select[name=unit]').value, details: String(!!s), open: String(s?.open), visible: String(s?.checkVisibility()),
  };
 })()`, &view)
	if view.Panel != "true" || view.Tab != "true" || view.Filter != "upload" || view.Details != "true" || view.Open != "true" || view.Visible != "true" {
		t.Fatalf("the restored view is %+v", view)
	}
}

func TestBrowserRestoredActivityHidesOtherUnitsEvents(t *testing.T) {
	t.Parallel()
	fresh, _ := openRestoredActivity(t)
	var restoredOther bool
	fresh.eval(`!!document.querySelector('#workstream-activity [data-turn=s13]')`, &restoredOther)
	if restoredOther {
		t.Fatal("the restored view shows another unit's events")
	}
}

func TestBrowserRestoredActivityKeepsTheLocation(t *testing.T) {
	t.Parallel()
	fresh, hash := openRestoredActivity(t)
	var restored string
	fresh.eval(`location.hash`, &restored)
	if restored != hash {
		t.Fatalf("the restored location is %q, want %q", restored, hash)
	}
}

type unreadPage struct {
	p        *page
	api      *fakeAPI
	reads    *counter
	baseFeed []string
	row      string
}

func openUnreadPage(t *testing.T) *unreadPage {
	t.Helper()
	api := newFakeAPI(2)
	api.setFeed("w_001", featureTransitionEntry(1, "building", "building"))
	reads := &counter{}
	base := api.handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/feed/w_001" {
			reads.add()
		}
		base.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.click(`[data-select="w_000"]`)
	p.awaitFeedKinds("")
	p.awaitNoUnread()
	reads.await(t, "the page never read the feed of w_001", func(n int) bool { return n > 0 })
	p.settle()
	return &unreadPage{p: p, api: api, reads: reads, baseFeed: []string{featureTransitionEntry(1, "building", "building")}, row: `[data-select="w_001"] [data-field=unread]`}
}

func (u *unreadPage) read(t *testing.T, what string, entries ...string) {
	t.Helper()
	before := u.reads.load()
	u.api.setFeed("w_001", entries...)
	u.api.pushEvent("resync")
	u.reads.await(t, "the page never read the feed after "+what, func(n int) bool { return n > before })
	u.p.settle()
}

func TestBrowserUnreadMarkersIgnoreActivityOnlyEntries(t *testing.T) {
	t.Parallel()
	u := openUnreadPage(t)
	entries := u.baseFeed
	for n, extra := range []string{
		sessionEntry(2, "upload", "running", ""),
		unitTransitionEntry(3, "upload", "ready", "implementing", "a mason started"),
		unitChecksEntry(4, "upload", 1, "failed"),
		sessionEntry(5, "upload", "failed", "no sandbox"),
	} {
		entries = append(entries, extra)
		u.read(t, fmt.Sprintf("activity-only event %d", n+1), entries...)
		u.p.awaitNoUnread()
	}
}

func TestBrowserUnreadMarkersCountConversationEntries(t *testing.T) {
	t.Parallel()
	u := openUnreadPage(t)
	entries := u.baseFeed
	for _, step := range []struct {
		what  string
		extra string
	}{
		{"a message", chatEntry("message", 6, "A note for the manager.")},
		{"a milestone", featureTransitionEntry(7, "assembled", "every unit merged")},
		{"a final review", finalReviewEntry(8, 1, "reviewed")},
	} {
		u.p.click(`[data-select="w_001"]`)
		u.p.awaitNoUnread()
		u.p.click(`[data-select="w_000"]`)
		entries = append(entries, step.extra)
		u.read(t, step.what, entries...)
		u.p.await("the workstream marked after "+step.what, `document.querySelector(`+quote(u.row)+`) !== null`)
	}
}

func TestBrowserOlderStatusesRecedeInActivity(t *testing.T) {
	t.Parallel()
	f := openFeedPage(t, 1, "", map[string][]string{"w_000": {
		statusEntry(1, "First status.", ""),
		statusEntry(2, "Second status.", ""),
		statusEntry(3, "Latest status.", "Rule on the upload API."),
	}})
	p := f.page
	p.click(`[data-tab=activity]`)
	p.await("three statuses", `document.querySelectorAll('#workstream-activity [data-kind=status]').length === 3`)
	var got struct {
		Current             string
		Old, Latest         float64
		OldSize, LatestSize float64
		Count               int
	}
	p.eval(`(() => {
  const latest = document.querySelector('#workstream-activity [data-kind=status][data-latest=true]');
  const old = document.querySelector('#workstream-activity [data-kind=status][data-latest=false]');
  return {
   current: latest.querySelector('[data-field=current]')?.textContent ?? '',
   latest: parseFloat(getComputedStyle(latest).opacity), old: parseFloat(getComputedStyle(old).opacity),
   latestSize: parseFloat(getComputedStyle(latest.querySelector('[data-field=note]')).fontSize),
   oldSize: parseFloat(getComputedStyle(old.querySelector('[data-field=note]')).fontSize),
   count: document.querySelectorAll('#workstream-activity [data-kind=status][data-latest=false] [data-field=current]').length,
  };
 })()`, &got)
	if got.Current != "Current" || got.Count != 0 {
		t.Fatalf("the latest status is marked %q, %d older ones are too", got.Current, got.Count)
	}
	if !(got.Old < got.Latest) || !(got.OldSize <= got.LatestSize) {
		t.Fatalf("older statuses do not recede: %+v", got)
	}
}

type proseMetrics struct {
	Cap, Paragraph, Pre, Table, Column float64
	PreFont, TableFont, CodeFont, Mono string
	Tabular                            string
}

func readProseMetrics(t *testing.T) proseMetrics {
	t.Helper()
	prose := strings.Repeat("The uploads resume where they stopped, and every chunk is checked. ", 12)
	table := "| Chunk | Offset | Checksum | Acknowledged by the store | Retried after a lost connection |\n| --- | --- | --- | --- | --- |\n| 0001 | 0 | 9f2c41aa | yes | no |\n| 0002 | 4194304 | 03be77d1 | yes | yes |"
	code := "```go\nfunc resume(ctx context.Context, upload Upload, offset int64) (int64, error) { return upload.Acknowledged(ctx, offset, checksum(upload.Chunk(offset))) }\n```"
	text := prose + "\n\n" + table + "\n\n" + code
	f := openFeedPage(t, 1, "", map[string][]string{"w_000": {chatEntry("response", 1, text)}})
	p := f.page
	p.await("the reply drawn", `document.querySelector('#workstreams [data-kind=response] .markdown pre')`)
	var m proseMetrics
	p.eval(`(() => {
  const reply = document.querySelector('#workstreams [data-kind=response] .markdown');
  const probe = document.createElement('div');
  probe.style.width = '70ch';
  reply.append(probe);
  const cap = probe.getBoundingClientRect().width;
  probe.remove();
  const pre = reply.querySelector('pre');
  const box = reply.querySelector('.markdown-table');
  return {
   cap, paragraph: reply.querySelector('p').getBoundingClientRect().width, pre: pre.getBoundingClientRect().width,
   table: box.getBoundingClientRect().width, column: reply.getBoundingClientRect().width,
   preFont: getComputedStyle(pre).fontFamily, tableFont: getComputedStyle(box).fontFamily, codeFont: getComputedStyle(pre.querySelector('code')).fontFamily,
   mono: getComputedStyle(document.documentElement).getPropertyValue('--font-mono').trim(), tabular: getComputedStyle(box).fontVariantNumeric,
  };
 })()`, &m)
	return m
}

func TestBrowserProseHoldsTheReadingWidth(t *testing.T) {
	t.Parallel()
	m := readProseMetrics(t)
	if m.Paragraph > m.Cap+1 || m.Paragraph < .9*m.Cap {
		t.Fatalf("a long paragraph is %.0fpx wide, want the reading width %.0fpx", m.Paragraph, m.Cap)
	}
}

func TestBrowserTablesAndCodeRunWiderThanProse(t *testing.T) {
	t.Parallel()
	m := readProseMetrics(t)
	if m.Pre <= m.Cap+100 || m.Table <= m.Cap+100 {
		t.Fatalf("code is %.0fpx and the table box %.0fpx wide, want them to run past %.0fpx", m.Pre, m.Table, m.Cap)
	}
}

func TestBrowserTablesAndCodeUseTheOneMonoFace(t *testing.T) {
	t.Parallel()
	m := readProseMetrics(t)
	if m.PreFont != m.Mono || m.TableFont != m.Mono || m.CodeFont != m.Mono {
		t.Fatalf("code uses %q, the table %q and inline code %q, want the one mono %q", m.PreFont, m.TableFont, m.CodeFont, m.Mono)
	}
}

func TestBrowserTablesUseTabularFigures(t *testing.T) {
	t.Parallel()
	m := readProseMetrics(t)
	if !strings.Contains(m.Tabular, "tabular-nums") {
		t.Fatalf("the table's numbers are %q, want tabular figures", m.Tabular)
	}
}
