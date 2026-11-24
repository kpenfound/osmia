package web

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func activityEntry(id, kind, workstream, phase, lastProgress, failure string) string {
	body := fmt.Sprintf(`{"id":%s,"kind":%s,"project":"p_fake","workstream":%s,"phase":%s,`+
		`"started_at":"2026-01-01T00:00:00Z","last_progress":%s`,
		quote(id), quote(kind), quote(workstream), quote(phase), quote(lastProgress))
	if failure != "" {
		body += `,"failure":` + quote(failure)
	}
	return body + "}"
}

type operationsPage struct {
	p   *page
	api *fakeAPI
}

func openOperationsPage(t *testing.T) operationsPage {
	t.Helper()
	api := newFakeAPI(1)
	current := time.Now().UTC().Format(time.RFC3339)
	const stale = "2020-01-01T00:00:00Z"
	api.setActivity(
		activityEntry("git-1", "git", "w_000", "rebasing onto upstream", current, ""),
		activityEntry("git-2", "git", "w_000", "cloning", stale, ""),
		activityEntry("cleanup-1", "archive_cleanup", "w_000", "removing the workstream's trace", stale, ""),
		activityEntry("check-1", "check", "w_000", "checking unit demo (run 1)", stale, ""),
		activityEntry("turn-1", "turn", "w_000", "mason turn t1", stale, "the sandbox could not be created"),
	)
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	return operationsPage{p: p, api: api}
}

func (o operationsPage) openPopover() {
	o.p.click("#activity-button")
	o.p.await("the activity popover", `document.getElementById('activity-popover').matches(':popover-open')`)
	o.p.await("five activity entries", `document.querySelectorAll('#activity-list [data-activity]').length === 5`)
}

func TestBrowserActivityButtonCountsOperationsAndMarksBlockedWork(t *testing.T) {
	t.Parallel()
	o := openOperationsPage(t)
	o.p.awaitText("#activity-count", "5")
	o.p.await("the activity button marked blocked", `document.getElementById('activity-button').dataset.blocked === 'true'`)
}

func TestBrowserActivityShowsACurrentOperationAsNotBlocked(t *testing.T) {
	t.Parallel()
	o := openOperationsPage(t)
	o.openPopover()
	o.p.await("the current git operation shown as not blocked", `document.querySelector('[data-activity="git-1"]').dataset.blocked === 'false'`)
}

func TestBrowserActivityShowsAStalledButBusyOperationAsNotBlocked(t *testing.T) {
	t.Parallel()
	o := openOperationsPage(t)
	o.openPopover()
	o.p.await("the stalled but merely busy git operation shown as not blocked", `document.querySelector('[data-activity="git-2"]').dataset.blocked === 'false'`)
	o.p.await("the stalled but merely busy check shown as not blocked", `document.querySelector('[data-activity="check-1"]').dataset.blocked === 'false'`)
}

func TestBrowserActivityShowsAStalledArchiveCleanupAsBlocked(t *testing.T) {
	t.Parallel()
	o := openOperationsPage(t)
	o.openPopover()
	o.p.await("the stalled archive cleanup shown as blocked", `document.querySelector('[data-activity="cleanup-1"]').dataset.blocked === 'true'`)
	o.p.awaitText(`[data-activity="cleanup-1"] [data-field="blocked"]`, "No progress")
}

func TestBrowserActivityShowsAFailedOperationAsBlockedWithItsFailure(t *testing.T) {
	t.Parallel()
	o := openOperationsPage(t)
	o.openPopover()
	o.p.await("the failed operation shown as blocked", `document.querySelector('[data-activity="turn-1"]').dataset.blocked === 'true'`)
	o.p.awaitText(`[data-activity="turn-1"] [data-field="failure"]`, "the sandbox could not be created")
}

func TestBrowserActivityEmptiesWhenNoOperationRuns(t *testing.T) {
	t.Parallel()
	o := openOperationsPage(t)
	o.openPopover()
	o.api.setActivity()
	o.api.pushEvent("activity")
	o.p.await("the activity list emptied", `document.querySelectorAll('#activity-list [data-activity]').length === 0`)
	o.p.awaitText("#activity-list", "Nothing is running.")
}

func TestBrowserActivityButtonIsNeitherActiveNorBlockedWhenNoOperationRuns(t *testing.T) {
	t.Parallel()
	o := openOperationsPage(t)
	o.p.awaitText("#activity-count", "5")
	o.api.setActivity()
	o.api.pushEvent("activity")
	o.p.await("the activity button no longer active or blocked",
		`document.getElementById('activity-button').dataset.active === 'false' && document.getElementById('activity-button').dataset.blocked === 'false'`)
}
