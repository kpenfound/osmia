package browsertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// confirmDestroy answers the native confirmation after one dispatched click.
func confirmDestroy(p *page, selector string, accept bool) {
	p.T.Helper()
	ctx, cancel := context.WithCancel(p.Ctx)
	defer cancel()
	type answer struct {
		message string
		err     error
	}
	answered := make(chan answer, 1)
	chromedp.ListenTarget(ctx, func(event any) {
		if dialog, ok := event.(*cdppage.EventJavascriptDialogOpening); ok {
			go func() {
				err := chromedp.Run(ctx, cdppage.HandleJavaScriptDialog(accept))
				answered <- answer{dialog.Message, err}
			}()
		}
	})
	p.Click(selector)
	select {
	case out := <-answered:
		must(p.T, out.err)
		for _, want := range []string{"local branch", "unpushed commits", "history", "permanently", "cannot be undone", "Pushed branches stay on the remote"} {
			if !strings.Contains(out.message, want) {
				p.T.Fatalf("destroy confirmation %q lacks %q", out.message, want)
			}
		}
	case <-time.After(browserTimeout):
		p.T.Fatal("destroy did not ask for confirmation")
	}
}

func assertDestroyRequests(p *page, route string, id config.WorkstreamID, want int) {
	p.T.Helper()
	path := Prefix + "/" + route + "/" + string(id) + "/destroy"
	got := 0
	for _, requested := range p.Paths() {
		if requested == path {
			got++
		}
	}
	if got != want {
		p.T.Fatalf("destroy requests: got %d, want %d", got, want)
	}
}

// The reply can be visible before its operation is acknowledged. Wait for
// that completion before dispatching destroy, rather than retrying a refusal.
func awaitDestroyTurn(t *testing.T, s *Service, project config.ProjectID, id config.WorkstreamID) {
	t.Helper()
	repository, err := service.InspectForTest(s).Repository(project)
	must(t, err)
	fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
		ops, err := repository.Operations(id)
		must(t, err)
		return ops
	})
	deadline := time.Now().Add(browserTimeout)
	for {
		running := service.InspectForTest(s).SessionTurnRunning(id)
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("completed turn remains active")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertBrowserDestroyed(p *page, s *Service, c *Client, project config.ProjectID, id config.WorkstreamID, route string) {
	p.T.Helper()
	p.Await("destroy leaves the conversation and sidebar", fmt.Sprintf(`
      !document.querySelector('[data-view=project-pane]').hidden &&
      !document.querySelector('[data-select=%q]') &&
      !document.querySelector('[data-pane-workstream=%q]') &&
      JSON.parse(localStorage.getItem('osmia.selected')) === null`, id, id))
	p.Await("conversation placeholders replace deleted history", `
      document.getElementById('design-feed').textContent === 'No design session selected.' &&
      document.getElementById('session-feed').textContent === 'No session selected.'`)
	ctx := context.Background()
	designs, err := c.Designs(ctx, project)
	must(p.T, err)
	for _, design := range designs.Designs {
		if design.Workstream == id {
			p.T.Fatal("destroyed design remains listed by the service")
		}
	}
	sessions, err := c.Sessions(ctx, project)
	must(p.T, err)
	for _, session := range sessions.Sessions {
		if session.Workstream == id {
			p.T.Fatal("destroyed session remains listed by the service")
		}
	}
	if route == "design" {
		_, err = c.Design(ctx, id)
		fixturekit.AssertCode(p.T, err, api.NotFound)
		_, err = c.DesignConversation(ctx, id)
		fixturekit.AssertCode(p.T, err, api.NotFound)
	} else {
		_, err = c.Session(ctx, id)
		fixturekit.AssertCode(p.T, err, api.NotFound)
	}
	gone, err := trace.WorkstreamTraceGone(service.InspectForTest(s).Config().Root, project, id)
	must(p.T, err)
	if !gone {
		p.T.Fatal("destroyed session retains its on-disk trace")
	}
	fixturekit.AssertGone(p.T, sessionWorktreePath(service.InspectForTest(s).Config().Root.String(), project, id))
	_, exists, err := service.InspectForTest(s).SessionGit(project).Branch(ctx, sessionBranch(id))
	must(p.T, err)
	if exists {
		p.T.Fatal("destroyed session retains its local branch")
	}
}

// serial: native confirmation dialogs use the shared browser window.
func TestBrowserDestroyDesignConfirmsRefusesBusyAndRemovesHistory(t *testing.T) {
	s, c, project, engine := newDesignEnforcementFixture(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseTurn := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseTurn)
	engine.Turns["*"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &agent.Result{ClaudeID: "design", ResultText: "Design discussion preserved."}, nil
	}
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Click("#sidebar-new-design")
	p.Choose("#design-add-form [name=project]", string(project))
	p.Click("#design-add-form [type=submit]")
	p.AwaitText("#design-add-form .result", "Opened design session")
	listed, err := c.Designs(context.Background(), project)
	must(t, err)
	if len(listed.Designs) != 1 {
		t.Fatalf("designs: %+v", listed)
	}
	id := listed.Designs[0].Workstream
	selectOutcomeDesign(p, id)

	confirmDestroy(p, "#design-destroy", false)
	assertDestroyRequests(p, "design", id, 0)
	_, err = c.Design(context.Background(), id)
	must(t, err)
	// Sending after cancellation proves that the retained session is usable.
	p.TypeInto("#design-form textarea", "Keep thinking")
	p.Click("#design-form [type=submit]")
	select {
	case <-entered:
	case <-time.After(browserTimeout):
		t.Fatal("design turn did not start")
	}
	confirmDestroy(p, "#design-destroy", true)
	p.AwaitText("#design-destroy-result[data-outcome=error]", "a turn or control is running")
	assertBrowserDesignSelection(t, p, c, id, false)
	p.Await("refused design stays in the sidebar", fmt.Sprintf(`!!document.querySelector('[data-select=%q]')`, id))
	_, err = c.Design(context.Background(), id)
	must(t, err)
	releaseTurn()
	p.AwaitText("#design-feed", "Design discussion preserved.")
	awaitDestroyTurn(t, s, project, id)
	confirmDestroy(p, "#design-destroy", true)
	assertBrowserDestroyed(p, s, c, project, id, "design")
	assertDestroyRequests(p, "design", id, 2)
}

// serial: native confirmation dialogs use the shared browser window.
func TestBrowserDestroySessionConfirmsRefusesBusyAndRemovesCommittedBranch(t *testing.T) {
	s, c, project, engine, _ := sessionWebFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseTurn := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseTurn)
	engine.Turn = func(ctx context.Context, _ agent.Request) (*agent.Result, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &agent.Result{ClaudeID: "session", ResultText: "Exploration preserved."}, nil
	}
	opened, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: project, Profile: "default", Title: "Explore deletion"})
	must(t, err)
	id := opened.Session.Workstream
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectSession(id)

	confirmDestroy(p, "#session-destroy", false)
	assertDestroyRequests(p, "sessions", id, 0)
	_, err = c.Session(context.Background(), id)
	must(t, err)
	p.TypeInto("#session-form textarea", "Keep exploring")
	p.Click("#session-form [type=submit]")
	select {
	case <-entered:
	case <-time.After(browserTimeout):
		t.Fatal("session turn did not start")
	}
	confirmDestroy(p, "#session-destroy", true)
	p.AwaitText("#session-control-result[data-outcome=error]", "a turn or control is running")
	p.Await("refused session stays selected and listed", fmt.Sprintf(`
      !document.querySelector('[data-view=session]').hidden &&
      document.getElementById('session-meta').textContent.includes(%q) &&
      !!document.querySelector('[data-select=%q]')`, id, id))
	_, err = c.Session(context.Background(), id)
	must(t, err)
	releaseTurn()
	fixturekit.AwaitSession(t, c, id, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	p.AwaitText("#session-feed", "Exploration preserved.")
	awaitDestroyTurn(t, s, project, id)
	worktree := sessionWorktreePath(service.InspectForTest(s).Config().Root.String(), project, id)
	must(t, os.WriteFile(filepath.Join(worktree, "note.txt"), []byte("unpublished exploration\n"), 0600))
	p.TypeInto("#session-commit-form [name=message]", "record exploration")
	p.Click("#session-commit-form [type=submit]")
	p.AwaitText("#session-commit-form .result[data-outcome=ok]", "Committed")
	p.AwaitText("#session-commits", "record exploration")
	_, exists, err := service.InspectForTest(s).SessionGit(project).Branch(context.Background(), sessionBranch(id))
	must(t, err)
	if !exists {
		t.Fatal("session commit did not leave a local branch")
	}
	confirmDestroy(p, "#session-destroy", true)
	assertBrowserDestroyed(p, s, c, project, id, "sessions")
	assertDestroyRequests(p, "sessions", id, 2)
}
