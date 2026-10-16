package service

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

	"github.com/kpenfound/osmia/internal/config"
)

// sessionWebFixture starts a service with a web listener and a registered
// project with a real local upstream remote, the production enforcement
// wiring bound to a fake engine (the same pattern newSessionChatFixture
// uses, without opening a session), so a browser test can open and drive a
// session through the real sandbox, thread and Git machinery.
func sessionWebFixture(t *testing.T) (*Service, *Client, config.ProjectID, *sessionEngine, string) {
	t.Helper()
	opts, clone := projectFixture(t)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("[listen]\nweb = \"127.0.0.1:0\"\n")
	must(t, err)
	must(t, configFile.Close())
	home := filepath.Dir(clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	demoGit(t, home, "-C", clone, "add", "README.md")
	demoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	demoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	demoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	engine := &sessionEngine{}
	enforced := Enforce(opts, Enforcement{Engine: engine})
	opts.Threads, opts.controls = enforced.Threads, enforced.controls
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	must(t, os.WriteFile(added.Project.Charter, []byte("1. Keep changes small.\n"), 0600))
	return s, c, added.Project.ID, engine, clone
}

// selectSession picks the session with id from the sidebar the way
// selectWorkstream does for an ordinary workstream, but waits for the
// session view's own content (there is no #workstream-head for a session).
func (p *page) selectSession(id config.WorkstreamID) {
	p.t.Helper()
	row := `[data-select="` + string(id) + `"]`
	p.await("the session "+string(id)+" in the list", `document.querySelector(`+quote(row)+`) !== null`)
	var collapsed bool
	p.eval(`getComputedStyle(document.getElementById('sidebar')).display === 'none'`, &collapsed)
	if collapsed {
		p.click("#picker-toggle")
	}
	p.click(row)
	p.await("the session "+string(id)+" shown", `!document.querySelector('.view[data-view="session"]').hidden &&
		document.getElementById('session-meta').textContent.includes(`+quote(string(id))+`)`)
}

// The "New session" form opens a session on a chosen project and profile; it
// appears in the workstream list marked with a session tag, and opens the
// session view, which shows that profile. Covers plan#sess-web acceptance
// item 1 and spec#22's "New session" form and session marker.
func TestBrowserOpenSessionThroughFormListsItAsASessionAndOpensItsView(t *testing.T) {
	t.Parallel()
	s, c, projectID, _, _ := sessionWebFixture(t)
	p := openBrowser(t)

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)

	p.click("#sidebar-new-session")
	p.await("the new session view", `!document.querySelector('.view[data-view="session-add"]').hidden`)
	p.await("the project choice", `[...document.querySelector('#session-add-form [name=project]').options].some(o => o.value === `+quote(string(projectID))+`)`)
	p.choose("#session-add-form [name=project]", string(projectID))
	p.await("the profile choice", `[...document.querySelector('#session-add-form [name=profile]').options].some(o => o.value === "default")`)
	p.choose("#session-add-form [name=profile]", "default")
	p.typeInto("#session-add-form [name=title]", "Investigate flaky test")
	p.click("#session-add-form [type=submit]")
	p.awaitText("#session-add-form .result", "Opened session")

	sessions, err := c.Sessions(context.Background(), projectID)
	must(t, err)
	if len(sessions.Sessions) != 1 {
		t.Fatalf("sessions after opening through the form: %+v", sessions.Sessions)
	}
	id := sessions.Sessions[0].Workstream

	p.await("the session marked with a session tag in the workstream list", `(() => {
		const row = document.querySelector(`+quote(`[data-select="`+string(id)+`"]`)+`);
		const tag = row ? row.querySelector('[data-field=kind]') : null;
		return tag !== null && !tag.hidden && tag.textContent === 'Session';
	})()`)

	p.await("the session view open", `!document.querySelector('.view[data-view="session"]').hidden`)
	p.awaitText("#session-title", "Investigate flaky test")
	p.awaitText("#session-meta", "default")
	p.awaitText("#session-meta", string(id))
	p.click("#project-home")
	p.await("the exploratory session outside any workgroup", `(() => {
      const row = document.querySelector('[data-sessions="session"] [data-pane-workstream="`+string(id)+`"]');
      return row && !row.closest('[data-workgroup]');
    })()`)
	p.click(`[data-drilldown="` + string(id) + `"]`)
	p.await("the exploratory session drill-down", `!document.querySelector('[data-view="session"]').hidden`)
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
	engine.turn = func(_ context.Context, req agent.Request) (*agent.Result, error) {
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
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.selectSession(id)

	// A sent message and its reply appear without a page reload.
	p.eval(`window.notReloaded = true`, nil)
	p.typeInto("#session-form textarea", "Hello there")
	p.click("#session-form [type=submit]")
	p.awaitText("#session-feed", "Hello there")
	p.awaitText("#session-feed", "Hi from the agent")
	p.await("the same document", `window.notReloaded === true`)
	var labelledChiefOfStaff bool
	p.eval(`[...document.querySelectorAll('#session-feed [data-ann=role]')].some((n) => n.textContent === 'Chief of staff')`, &labelledChiefOfStaff)
	if labelledChiefOfStaff {
		t.Fatal("the session feed labels a reply \"Chief of staff\"; a session has no chief of staff")
	}

	// A second owner message sent while the session's turn is still in
	// flight is refused as busy; it is fired as a raw request (not through
	// the composer's own button, which the UI disables for the duration of
	// its own request) so the refusal is genuine, not merely the composer
	// disabling itself.
	p.eval(`(() => { fetch(`+quote("/v1/sessions/"+string(id)+"/message")+`, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({text: 'Hold please'})}).catch(() => {}); })()`, nil)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocking session turn never started")
	}
	p.typeInto("#session-form textarea", "Are you there?")
	p.click("#session-form [type=submit]")
	p.awaitText("#session-result", "turn is running")

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
func TestBrowserSessionViewShowsControlsAndNoWorkstreamPanels(t *testing.T) {
	t.Parallel()
	s, c, projectID, _, clone := sessionWebFixture(t)
	resp, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: projectID, Profile: "default"})
	must(t, err)
	id := resp.Session.Workstream

	p := openBrowser(t)
	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.selectSession(id)

	// The profile shows, with no control to change it.
	p.awaitText("#session-meta", "default")
	var profileControl bool
	p.eval(`document.querySelectorAll('.view[data-view="session"] select').length > 0 ||
		document.querySelector('.view[data-view="session"] [data-action*=profile]') !== null`, &profileControl)
	if profileControl {
		t.Fatal("the session view offers a control to change the profile")
	}

	// No spec, plan, unit, review or delivery panel shows: those live only
	// in the ordinary workstream view's tabbed panels and documents, trace
	// and action forms, none of which are nested in the session view.
	var otherPanels bool
	p.eval(`document.querySelector('.view[data-view="session"] [data-panel]') !== null ||
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
	p.eval(`document.getElementById('session-stop').disabled`, &stopDisabled)
	if !stopDisabled {
		t.Fatal("the stop control is enabled while the session is idle")
	}

	// A file written straight into the worktree shows as an uncommitted
	// change once the sidebar row is reselected, which reads the session's
	// changes again.
	worktree := sessionWorktreePath(s.cfg.Root.String(), projectID, id)
	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("hi\n"), 0600))
	p.click(`[data-select="` + string(id) + `"]`)
	p.awaitText("#session-diff", "note.txt")

	// The commit form commits it onto the session branch.
	p.typeInto("#session-commit-form [name=message]", "add a note")
	p.click("#session-commit-form [type=submit]")
	p.awaitText("#session-commit-form .result", "Committed")
	p.awaitText("#session-commits", "add a note")

	// The check button runs a check and shows its latest result, naming
	// whether it was smart or full; JEV is not enabled in this fixture, so
	// it is full.
	p.click("#session-check")
	p.awaitText("#session-check-result", "full")

	// Push sends the session branch to the project's local remote.
	home := filepath.Dir(clone)
	p.click("#session-push")
	p.awaitText("#session-control-result", "Pushed")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	if got := strings.TrimSpace(demoGit(t, home, "-C", bare, "rev-parse", sessionBranch(id))); got == "" {
		t.Fatalf("the session branch did not reach the remote")
	}

	// End releases the worktree and marks the session ended; its controls
	// then block.
	p.click("#session-end")
	p.awaitText("#session-control-result", "ended")
	p.awaitText("#session-meta", "ended")
	for _, selector := range []string{"#session-stop", "#session-check", "#session-push", "#session-end"} {
		var disabled bool
		p.eval(`document.querySelector(`+quote(selector)+`).disabled`, &disabled)
		if !disabled {
			t.Fatalf("%s is enabled after the session ended", selector)
		}
	}
}
