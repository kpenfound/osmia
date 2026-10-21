package browsertest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/service"
)

// The "New session" form opens a session on a chosen project and profile; it
// appears in the workstream list marked with a session tag, and opens the
// session view, which shows that profile. Covers plan#sess-web acceptance
// item 1 and spec#22's "New session" form and session marker.
func TestBrowserOpenSessionThroughFormListsItAsASessionAndOpensItsView(t *testing.T) {
	t.Parallel()
	s, c, projectID, _, _ := sessionWebFixture(t)
	p := openBrowser(t)

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)

	p.Click("#sidebar-new-session")
	p.Await("the new session view", `!document.querySelector('.view[data-view="session-add"]').hidden`)
	p.Await("the project choice", `[...document.querySelector('#session-add-form [name=project]').options].some(o => o.value === `+quote(string(projectID))+`)`)
	p.Choose("#session-add-form [name=project]", string(projectID))
	p.Await("the profile choice", `[...document.querySelector('#session-add-form [name=profile]').options].some(o => o.value === "default")`)
	p.Choose("#session-add-form [name=profile]", "default")
	p.TypeInto("#session-add-form [name=title]", "Investigate flaky test")
	p.Click("#session-add-form [type=submit]")
	p.AwaitText("#session-add-form .result", "Opened session")

	sessions, err := c.Sessions(context.Background(), projectID)
	must(t, err)
	if len(sessions.Sessions) != 1 {
		t.Fatalf("sessions after opening through the form: %+v", sessions.Sessions)
	}
	id := sessions.Sessions[0].Workstream

	p.Await("the session marked with a session tag in the workstream list", `(() => {
		const row = document.querySelector(`+quote(`[data-select="`+string(id)+`"]`)+`);
		const tag = row ? row.querySelector('[data-field=kind]') : null;
		return tag !== null && !tag.hidden && tag.textContent === 'Session';
	})()`)

	p.Await("the session view open", `!document.querySelector('.view[data-view="session"]').hidden`)
	p.AwaitText("#session-title", "Investigate flaky test")
	p.AwaitText("#session-meta", "default")
	p.AwaitText("#session-meta", string(id))
	p.Click("#project-home")
	p.Await("the exploratory session outside any workgroup", `(() => {
      const row = document.querySelector('[data-sessions="session"] [data-pane-workstream="`+string(id)+`"]');
      return row && !row.closest('[data-workgroup]');
    })()`)
	p.Click(`[data-drilldown="` + string(id) + `"]`)
	p.Await("the exploratory session drill-down", `!document.querySelector('[data-view="session"]').hidden`)
}

// Sending a message to a session posts it and shows the reply, once the
// turn finishes, without a page reload, and a message sent while a
// previous turn is still in flight is refused with a busy error the
// composer shows. Covers plan#sess-web acceptance item 2 and spec#22's chat
// behaviour.
func TestBrowserSessionSendAndReplyAppearWithoutReloadAndBusyRefusal(t *testing.T) {
	t.Parallel()
	s, c, projectID, engine, _ := sessionWebFixture(t)
	resp, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: projectID, Profile: "default"})
	must(t, err)
	id := resp.Session.Workstream

	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	// The runner wraps the owner's text in a history-replay prefix (see
	// internal/coreadapter/execution.go: req.Prompt = t.History + "\n\n" +
	// t.Prompt), so the fake engine matches on substrings of the owner's
	// own text rather than an exact prompt, and replies with fixed text
	// that does not depend on that wrapping.
	var releaseOnce sync.Once
	releaseTurn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseTurn)
	engine.Turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
		switch {
		case strings.Contains(req.Prompt, "Hold please"):
			close(entered)
			defer close(finished)
			<-release
			return &agent.Result{ClaudeID: "model-session-1", ResultText: "Reply to hold please"}, nil
		case strings.Contains(req.Prompt, "Hello there"):
			return &agent.Result{ClaudeID: "model-session-1", ResultText: "Hi from the agent"}, nil
		default:
			return &agent.Result{ClaudeID: "model-session-1", ResultText: "Reply"}, nil
		}
	}

	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectSession(id)

	// A sent message and its reply appear without a page reload.
	p.Eval(`window.notReloaded = true`, nil)
	p.TypeInto("#session-form textarea", "Hello there")
	p.Click("#session-form [type=submit]")
	p.AwaitText("#session-feed", "Hello there")
	p.AwaitText("#session-feed", "Hi from the agent")
	p.Await("the same document", `window.notReloaded === true`)
	var labelledChiefOfStaff bool
	p.Eval(`[...document.querySelectorAll('#session-feed [data-ann=role]')].some((n) => n.textContent === 'Chief of staff')`, &labelledChiefOfStaff)
	if labelledChiefOfStaff {
		t.Fatal("the session feed labels a reply \"Chief of staff\"; a session has no chief of staff")
	}

	// A second owner message sent while the session's turn is still in
	// flight is refused as busy; it is fired as a raw request (not through
	// the composer's own button, which the UI disables for the duration of
	// its own request) so the refusal is genuine, not merely the composer
	// disabling itself.
	p.Eval(`(() => { fetch(`+quote("/v1/sessions/"+string(id)+"/message")+`, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({text: 'Hold please'})}).catch(() => {}); })()`, nil)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocking session turn never started")
	}
	p.TypeInto("#session-form textarea", "Are you there?")
	p.Click("#session-form [type=submit]")
	p.AwaitText("#session-result", "turn is running")

	releaseTurn()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocking session turn never finished")
	}
}

// The session view shows its fixed profile with no control to change it,
// and offers the stop, changes, commit, check, push and end controls,
// which work end to end against a real local worktree and remote, with no
// spec, plan, unit, review or delivery panel. Covers plan#sess-web
// acceptance item 3 and spec#22's control list.
// serial: native confirmation dialogs use the shared browser window.
func TestBrowserSessionViewShowsControlsAndNoWorkstreamPanels(t *testing.T) {
	s, c, projectID, _, clone := sessionWebFixture(t)
	resp, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: projectID, Profile: "default"})
	must(t, err)
	id := resp.Session.Workstream

	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectSession(id)

	// The profile shows, with no control to change it.
	p.AwaitText("#session-meta", "default")
	var profileControl bool
	p.Eval(`document.querySelectorAll('.view[data-view="session"] select').length > 0 ||
		document.querySelector('.view[data-view="session"] [data-action*=profile]') !== null`, &profileControl)
	if profileControl {
		t.Fatal("the session view offers a control to change the profile")
	}

	// No spec, plan, unit, review or delivery panel shows: those live only
	// in the ordinary workstream view's tabbed panels and documents, trace
	// and action forms, none of which are nested in the session view.
	var otherPanels bool
	p.Eval(`document.querySelector('.view[data-view="session"] [data-panel]') !== null ||
		document.querySelector('.view[data-view="session"] .tabs') !== null ||
		document.querySelector('.view[data-view="session"] #documents-form') !== null ||
		document.querySelector('.view[data-view="session"] #trace-form') !== null ||
		document.querySelector('.view[data-view="session"] #workstream-action') !== null`, &otherPanels)
	if otherPanels {
		t.Fatal("the session view shows a spec, plan, unit, review or delivery panel")
	}

	// The stop control is present and disabled while the session is idle
	// (it is not currently in a turn).
	var stopDisabled bool
	p.Eval(`document.getElementById('session-stop').disabled`, &stopDisabled)
	if !stopDisabled {
		t.Fatal("the stop control is enabled while the session is idle")
	}

	// A file written straight into the worktree shows as an uncommitted
	// change once the sidebar row is reselected, which reads the session's
	// changes again.
	worktree := sessionWorktreePath(service.InspectForTest(s).Config().Root.String(), projectID, id)
	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
	p.Click(`[data-select="` + string(id) + `"]`)
	p.AwaitText("#session-diff", "note.txt")

	// The commit form commits it onto the session branch.
	p.TypeInto("#session-commit-form [name=message]", "add a note")
	p.Click("#session-commit-form [type=submit]")
	p.AwaitText("#session-commit-form .result", "Committed")
	p.AwaitText("#session-commits", "add a note")

	// The check button runs a check and shows its latest result, naming
	// whether it was smart or full; JEV is not enabled in this fixture, so
	// it is full.
	p.Click("#session-check")
	p.AwaitText("#session-check-result", "full")

	// Push sends the session branch to the project's local remote.
	home := filepath.Dir(clone)
	p.Click("#session-push")
	p.AwaitText("#session-control-result", "Pushed")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	if got := strings.TrimSpace(demoGit(t, home, "-C", bare, "rev-parse", sessionBranch(id))); got == "" {
		t.Fatalf("the session branch did not reach the remote")
	}

	// End releases the worktree and marks the session ended; its controls
	// then block. Destroy remains available for the ended session.
	p.Click("#session-end")
	p.AwaitText("#session-control-result", "ended")
	p.AwaitText("#session-meta", "ended")
	for _, selector := range []string{"#session-stop", "#session-check", "#session-push", "#session-end"} {
		var disabled bool
		p.Eval(`document.querySelector(`+quote(selector)+`).disabled`, &disabled)
		if !disabled {
			t.Fatalf("%s is enabled after the session ended", selector)
		}
	}
	confirmDestroy(p, "#session-destroy", true)
	assertBrowserDestroyed(p, s, c, projectID, id, "sessions")
	assertDestroyRequests(p, "sessions", id, 1)
	if got := strings.TrimSpace(demoGit(t, home, "-C", bare, "rev-parse", sessionBranch(id))); got == "" {
		t.Fatal("destroy removed the pushed branch from the test remote")
	}
}
