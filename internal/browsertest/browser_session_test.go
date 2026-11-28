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

func TestBrowserOpenSessionThroughFormListsItAsASessionAndOpensItsView(t *testing.T) {
	t.Parallel()
	s, c, projectID, _, _ := sessionWebFixture(t)
	p := openBrowser(t)

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)

	p.OpenNewItem("#sidebar-new-session")
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

func TestBrowserSessionSendAndReplyAppearWithoutReloadAndBusyRefusal(t *testing.T) {
	t.Parallel()
	s, c, projectID, engine, _ := sessionWebFixture(t)
	resp, err := c.OpenSession(context.Background(), service.SessionOpenRequest{Project: projectID, Profile: "default"})
	must(t, err)
	id := resp.Session.Workstream

	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
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

	p.Eval(`window.notReloaded = true`, nil)
	p.TypeInto("#session-form textarea", "Hello there")
	p.Click("#session-form [type=submit]")
	p.AwaitText("#session-feed", "Hello there")
	p.AwaitText("#session-feed", "Hi from the agent")
	p.Await("the same document", `window.notReloaded === true`)
	var labelledManager bool
	p.Eval(`[...document.querySelectorAll('#session-feed [data-ann=role]')].some((n) => n.textContent === 'Manager')`, &labelledManager)
	if labelledManager {
		t.Fatal("the session feed labels a reply \"Manager\"; a session has no manager")
	}

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

func TestBrowserSessionViewShowsControlsAndNoWorkstreamPanels(t *testing.T) {
	t.Parallel()
	s, c, projectID, _, clone := sessionWebFixture(t)
	resp, err := c.OpenSession(context.Background(), service.SessionOpenRequest{Project: projectID, Profile: "default"})
	must(t, err)
	id := resp.Session.Workstream

	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectSession(id)

	p.AwaitText("#session-meta", "default")
	var profileControl bool
	p.Eval(`document.querySelectorAll('.view[data-view="session"] select').length > 0 ||
		document.querySelector('.view[data-view="session"] [data-action*=profile]') !== null`, &profileControl)
	if profileControl {
		t.Fatal("the session view offers a control to change the profile")
	}

	var otherPanels bool
	p.Eval(`document.querySelector('.view[data-view="session"] [data-panel]') !== null ||
		document.querySelector('.view[data-view="session"] .tabs') !== null ||
		document.querySelector('.view[data-view="session"] #documents-form') !== null ||
		document.querySelector('.view[data-view="session"] #trace-form') !== null ||
		document.querySelector('.view[data-view="session"] #workstream-action') !== null`, &otherPanels)
	if otherPanels {
		t.Fatal("the session view shows a spec, plan, unit, review or delivery panel")
	}

	var stopDisabled bool
	p.Eval(`document.getElementById('session-stop').disabled`, &stopDisabled)
	if !stopDisabled {
		t.Fatal("the stop control is enabled while the session is idle")
	}

	worktree := sessionWorktreePath(service.InspectForTest(s).Config().Root.String(), projectID, id)
	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
	p.Click(`[data-select="` + string(id) + `"]`)
	p.AwaitText("#session-diff", "note.txt")

	p.TypeInto("#session-commit-form [name=message]", "add a note")
	p.Click("#session-commit-form [type=submit]")
	p.AwaitText("#session-commit-form .result", "Committed")
	p.AwaitText("#session-commits", "add a note")

	p.Click("#session-check")
	p.AwaitText("#session-check-result", "full")

	home := filepath.Dir(clone)
	p.Click("#session-push")
	p.AwaitText("#session-control-result", "Pushed")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	if got := strings.TrimSpace(demoGit(t, home, "-C", bare, "rev-parse", sessionBranch(id))); got == "" {
		t.Fatalf("the session branch did not reach the remote")
	}

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
