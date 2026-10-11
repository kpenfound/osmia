package web

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// activityEntry is the JSON of one backend activity entry, shaped the way
// the real service answers GET /v1/activity, with the given last-progress
// time and, when failure is not empty, a recorded failure.
func activityEntry(id, kind, workstream, phase, lastProgress, failure string) string {
	body := fmt.Sprintf(`{"id":%s,"kind":%s,"project":"p_fake","workstream":%s,"phase":%s,`+
		`"started_at":"2026-01-01T00:00:00Z","last_progress":%s`,
		quote(id), quote(kind), quote(workstream), quote(phase), quote(lastProgress))
	if failure != "" {
		body += `,"failure":` + quote(failure)
	}
	return body + "}"
}

// TestBrowserActivityShowsBackendOperationsIncludingBlockedAndFailed covers
// the web UI's half of spec#4: the page shows the backend activity record,
// including an operation that is blocked (no progress for a while) or has
// failed, and an operation whose last progress is current shows as neither.
// A turn or a check with stale progress but no failure shows as neither too:
// those kinds report only start and finish and routinely run for minutes,
// the same exemption from the no-progress rule the service's own health
// check applies (app.js, activityStallTrackedKinds; service's activity.go,
// stallTracked).
func TestBrowserActivityShowsBackendOperationsIncludingBlockedAndFailed(t *testing.T) {
	api := newFakeAPI(1)
	current := time.Now().UTC().Format(time.RFC3339)
	const stale = "2020-01-01T00:00:00Z"
	api.setActivity(
		activityEntry("git-1", "git", "w_000", "rebasing onto upstream", current, ""),
		activityEntry("git-2", "git", "w_000", "cloning", stale, ""),
		activityEntry("check-1", "check", "w_000", "checking unit demo (run 1)", stale, ""),
		activityEntry("turn-1", "turn", "w_000", "mason turn t1", stale, "the sandbox could not be created"),
	)
	server := httptest.NewServer(api.handler())
	t.Cleanup(server.Close)
	p := openBrowser(t)

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate(server.URL+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)

	p.awaitText("#activity-count", "4")
	p.await("the activity button marked blocked", `document.getElementById('activity-button').dataset.blocked === 'true'`)

	p.click("#activity-button")
	p.await("the activity popover", `document.getElementById('activity-popover').matches(':popover-open')`)
	p.await("four activity entries", `document.querySelectorAll('#activity-list [data-activity]').length === 4`)

	p.await("the current operation shown as not blocked",
		`document.querySelector('[data-activity="git-1"]').dataset.blocked === 'false'`)
	p.await("the stalled git operation shown as blocked",
		`document.querySelector('[data-activity="git-2"]').dataset.blocked === 'true'`)
	p.awaitText(`[data-activity="git-2"] [data-field="blocked"]`, "no progress")

	p.await("the stalled but merely busy check shown as not blocked",
		`document.querySelector('[data-activity="check-1"]').dataset.blocked === 'false'`)

	p.await("the failed operation shown as blocked",
		`document.querySelector('[data-activity="turn-1"]').dataset.blocked === 'true'`)
	p.awaitText(`[data-activity="turn-1"] [data-field="failure"]`, "the sandbox could not be created")

	// A later read with nothing running clears the list and the button.
	api.setActivity()
	api.pushEvent("activity")
	p.await("the activity list emptied", `document.querySelectorAll('#activity-list [data-activity]').length === 0`)
	p.awaitText("#activity-list", "Nothing is running.")
	p.await("the activity button no longer active or blocked",
		`document.getElementById('activity-button').dataset.active === 'false' && document.getElementById('activity-button').dataset.blocked === 'false'`)
}
