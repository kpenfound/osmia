package browsertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBrowserDesignBriefRefreshAndPinnedStart(t *testing.T) { browserDesignBrief(t, false) }
func TestBrowserDesignBriefConflictRefreshesDisplayedRevision(t *testing.T) {
	browserDesignBrief(t, true)
}

func browserDesignBrief(t *testing.T, conflict bool) {
	t.Helper()
	p := openBrowser(t)
	s, c, home, engine := newDesignEnforcementFixture(t, true)
	ctx := context.Background()
	clone := filepath.Join(t.TempDir(), "client")
	demoGit(t, filepath.Dir(clone), "init", "--quiet", clone)
	must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("client\n"), 0600))
	demoGit(t, filepath.Dir(clone), "-C", clone, "add", "README.md")
	demoGit(t, filepath.Dir(clone), "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	added, err := c.AddProject(ctx, ProjectAddRequest{Name: "Client", Upstream: "owner/client", Clone: clone})
	must(t, err)
	other := added.Project.ID
	writeCharter(t, c, other, "1. Keep the client compatible.\n")
	homeName := service.InspectForTest(s).Config().For(home).Project.Name
	var turn atomic.Int32
	engine.Mu.Lock()
	engine.Turns["*"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		revision := turn.Add(1)
		projects := []config.ProjectID{home}
		if revision > 1 {
			projects = append(projects, other)
		}
		content := fmt.Sprintf("Decisions: **revision %d**.\nIntent and tradeoffs: compatibility.\nRejected options: replacement.", revision)
		out, err := callTool(ctx, tools, draftBriefToolName, map[string]any{"content": content, "projects": projects})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("draft: %s %v", out, err)
		}
		return &agent.Result{ClaudeID: "brief", ResultText: fmt.Sprintf("Recorded brief %d.", revision)}, nil
	}
	engine.Mu.Unlock()
	opened, err := c.OpenDesign(ctx, home)
	must(t, err)
	id := opened.Design.Workstream
	_, err = c.DesignMessage(ctx, id, "Draft the initial brief.")
	must(t, err)
	awaitDesignConversation(t, s, c, id, func(r ConversationResponse) bool { return len(r.Entries) == 2 && r.Entries[1].State == TurnDone })
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/#workstream="+string(id)))
	p.Await("live", `document.body.dataset.connection === 'live'`)
	p.SelectDesign(id)
	p.AwaitText("#design-brief", "Brief revision: 1")
	p.AwaitText("#design-brief", homeName+" (home)")
	p.Await("read-only collapsed brief above Start", `document.querySelector('#design-brief details') && !document.querySelector('#design-brief details').open &&
  document.querySelector('#design-brief strong')?.textContent === 'revision 1' &&
  !document.querySelector('#design-brief input, #design-brief select, #design-brief textarea, #design-brief [contenteditable]') &&
  !!(document.querySelector('#design-brief').compareDocumentPosition(document.querySelector('#design-start')) & Node.DOCUMENT_POSITION_FOLLOWING)`)
	// Retain an actually displayed snapshot to exercise the race between
	// an owner's read and the service's revision check, using real responses.
	p.Eval(`window.notReloaded = true;
  window.briefFetch = window.fetch.bind(window);
  window.startPins = [];
  window.freezeBrief = false;
  window.fetch = async (url, init) => {
   if (url.endsWith('/start') && init?.method === 'POST') {
    window.startPins.push(JSON.parse(init.body));
    window.freezeBrief = false;
   }
   if (url === '/v1/design/`+string(id)+`' && (!init?.method || init.method === 'GET')) {
    if (window.freezeBrief && window.briefSnapshot) return new Response(window.briefSnapshot, {status:200});
    const response = await window.briefFetch(url,init);
    window.briefSnapshot = await response.clone().text();
    return response;
   }
   return window.briefFetch(url,init);
  };`, nil)
	// Capture revision one through the installed wrapper before freezing.
	p.Eval(`fetch('/v1/design/`+string(id)+`').then(() => { window.freezeBrief = `+fmt.Sprint(conflict)+`; window.snapshotReady = true; })`, nil)
	p.Await("snapshot ready", `window.snapshotReady`)
	_, err = c.DesignMessage(ctx, id, "Add Client and update the brief.")
	must(t, err)
	awaitDesignConversation(t, s, c, id, func(r ConversationResponse) bool { return len(r.Entries) == 4 && r.Entries[3].State == TurnDone })
	if conflict {
		p.AwaitText("#design-feed", "Recorded brief 2.")
		p.AwaitText("#design-brief", "Brief revision: 1")
		p.Await("Start enabled for the displayed snapshot", `!document.querySelector('#design-start').disabled`)
		p.Click("#design-start")
		p.AwaitText("#design-start-result", "the brief has changed")
		p.Await("error outcome", `document.querySelector('#design-start-result').dataset.outcome === 'error'`)
		current, err := c.Design(ctx, id)
		must(t, err)
		if current.Design.BriefRevision != 2 {
			t.Fatalf("design after conflict: %+v", current)
		}
		assertBrowserDesignSelection(t, p, c, id, false)
	} else {
		p.AwaitText("#design-brief", "Brief revision: 2")
		p.AwaitText("#design-brief", "Client")
		awaitBrowserDesignBrief(t, p, c, id)
		p.Click("#design-start")
		p.Await("the started workgroup view", `!document.querySelector('[data-view="workgroup"]').hidden`)
		p.AwaitText(`[data-view="workgroup"]`, "Client")
	}
	expected := 2
	if conflict {
		expected = 1
		p.AwaitText("#design-brief", "Brief revision: 2")
		p.AwaitText("#design-brief", "Client")
	}
	p.Await("Start pins the displayed revision without reload", fmt.Sprintf(`window.notReloaded && window.startPins.length === 1 && window.startPins[0].brief_revision === %d`, expected))
}
