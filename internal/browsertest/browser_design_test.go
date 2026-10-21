package browsertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestBrowserPageOpensADesignSessionConversesAndRefusesAnUndraftedStart
// covers the web page's surface of spec#6 to spec#8: the owner opens a
// design session from the project pane for an onboarded project, converses with
// it, and is refused, with the refusal shown, when starting a workgroup
// before the agent has drafted a brief. No agent runs in this test: the
// readiness pass and the brief it produces are covered end to end by
// internal/service's own TestDesignSessionProductionEnforcementGrantsOnlyReadOnlyTools,
// TestDraftBriefRefusesAnIncompleteBriefAndRecordsTheLatest and
// TestStartDesignWorkgroupHandsOffBriefOnTheSameChiefOfStaffThread.
func TestBrowserPageOpensADesignSessionConversesAndRefusesAnUndraftedStart(t *testing.T) {
	p := openBrowser(t)
	opts, clone := projectFixture(t)
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(data, []byte(listenWeb)...), 0600))
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")

	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)

	p.Click(`[data-project="` + string(added.Project.ID) + `"] [data-project-action="design-add"]`)
	p.Await("the design action selects its project", `document.querySelector('#design-add-form [data-project-select]').value === `+quote(string(added.Project.ID)))
	p.Click("#design-add-form [type=submit]")
	p.AwaitText("#design-add-form .result", "Opened design session")
	p.Await("the design view shown", `!document.querySelector('[data-view="design"]').hidden`)
	listed, err := c.Designs(context.Background(), added.Project.ID)
	must(t, err)
	if len(listed.Designs) != 1 {
		t.Fatalf("designs: %+v", listed)
	}
	stream := listed.Designs[0].Workstream
	assertBrowserDesignSelection(t, p, c, stream, false)

	// No brief has been drafted, since no agent turn ran: starting a
	// workgroup is refused, and the refusal is shown next to the button.
	p.Click("#design-start")
	p.AwaitText("#design-start-result", "has not drafted a brief yet")
	assertBrowserDesignSelection(t, p, c, stream, false)

	p.TypeInto("#design-form [name=text]", "What should we build?")
	p.Click("#design-form [type=submit]")
	p.AwaitText("#design-result", "the design agent answers in its next turn")
	p.Await("the message shown in the feed", `document.getElementById('design-feed').textContent.includes('What should we build?')`)
	assertBrowserDesignSelection(t, p, c, stream, false)
}

func TestBrowserDesignReadinessAssumptionsAndStartWorkgroup(t *testing.T) {
	p := openBrowser(t)
	s, c, project, engine := newDesignEnforcementFixture(t, true)
	const brief = "Decisions: keep the existing endpoint.\nIntent and tradeoffs: favor compatibility over flexibility.\nRejected options: a breaking replacement."
	const readiness = "Readiness: Should the public endpoint stay compatible?\nRecommended answer: Yes.\n<details><summary>Assumptions</summary>Use existing naming conventions. Reply to override.</details>\n\n"
	allowReply := make(chan struct{})
	engine.Turns["*"] = func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		out, err := callTool(ctx, tools, draftBriefToolName, map[string]any{"content": brief})
		if err != nil || !strings.Contains(out, `"recorded":true`) {
			return nil, fmt.Errorf("draft: %s %v", out, err)
		}
		select {
		case <-allowReply:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &agent.Result{ClaudeID: "design-session", ResultText: readiness + brief}, nil
	}
	// Hold the successful open response while an older status snapshot
	// lists the new workstream without its design marker. This makes the
	// creation/selection race reproducible, independent of request timing.
	p.Run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(`
          const fetchDesign = window.fetch.bind(window);
          window.designSnapshots = [];
          let opening = false;
          let release;
          window.fetch = async (url, init) => {
            const post = url === '/v1/design' && init?.method === 'POST';
            if (post) opening = true;
            const response = await fetchDesign(url, init);
            if (url === '/v1/status' && opening) {
              const status = await response.json();
              for (const w of status.workstreams) {
                if (w.kind === 'design') {
                  w.kind = '';
                  delete w.state;
                  window.designSnapshots.push({id: w.workstream, kind: w.kind, state: w.state,
                    selected: localStorage.getItem('osmia.selected')});
                }
              }
              return new Response(JSON.stringify(status), {status: response.status});
            }
            if (post) await new Promise(resolve => {
              release = resolve;
              window.releaseDesignOpen = () => { opening = false; release(); };
            });
            return response;
          };`).Do(ctx)
		return err
	}), chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Click("#sidebar-new-design")
	p.Choose("#design-add-form [data-project-select]", string(project))
	p.Click("#design-add-form [type=submit]")
	p.Await("the older status snapshot rendered before open completes", `window.designSnapshots.length &&
      document.querySelector('[data-select="' + window.designSnapshots.at(-1).id + '"] [data-field="kind"]')?.hidden &&
      typeof window.releaseDesignOpen === 'function'`)
	p.Run(chromedp.Evaluate(`window.releaseDesignOpen()`, nil))
	p.Await("the design view", `!document.querySelector('[data-view="design"]').hidden`)
	listed, err := c.Designs(context.Background(), project)
	must(t, err)
	if len(listed.Designs) != 1 {
		t.Fatalf("designs: %+v", listed)
	}
	stream := listed.Designs[0].Workstream
	assertBrowserDesignSelection(t, p, c, stream, false)
	p.TypeInto("#design-form [name=text]", "Preserve compatibility. Infer the naming conventions.")
	p.Click("#design-form [type=submit]")
	p.AwaitText("#design-result", "the design agent answers in its next turn")
	p.AwaitText("#design-feed", "Preserve compatibility. Infer the naming conventions.")
	assertBrowserDesignSelection(t, p, c, stream, false)
	close(allowReply)
	p.AwaitText("#design-feed", "Recommended answer: Yes.")
	p.Await("collapsed assumptions", `document.querySelector('#design-feed details') && !document.querySelector('#design-feed details').open`)
	p.Click("#design-feed summary")
	p.AwaitText("#design-feed details", "Use existing naming conventions.")
	p.AwaitText("#design-feed", "Rejected options: a breaking replacement.")
	assertBrowserDesignSelection(t, p, c, stream, false)
	p.Click("#design-start")
	var started bool
	p.Await("workgroup start", `document.getElementById('design-start-result').textContent.includes('Started a workgroup')`)
	assertBrowserDesignSelection(t, p, c, stream, true)
	p.AwaitText(`[data-view="workstream"]`, "Recommended answer: Yes.")
	conversation, err := c.Conversation(context.Background(), config.WorkstreamID(stream))
	must(t, err)
	for _, entry := range conversation.Entries {
		started = started || strings.Contains(entry.Text, "Recommended answer: Yes.")
	}
	if !started {
		t.Fatal("the design conversation did not continue on the workgroup thread")
	}
}

func assertBrowserDesignSelection(t *testing.T, p *page, c *Client, stream config.WorkstreamID, started bool) {
	t.Helper()
	w, err := c.Status(context.Background(), stream)
	must(t, err)
	view := "design"
	if started {
		view = "workstream"
		if w.Kind == "design" || w.State == nil || *w.State == "" {
			t.Fatalf("started design status: %+v", w)
		}
	} else if w.Kind != "design" || w.State != nil {
		t.Fatalf("unstarted design status: %+v", w)
	}
	p.Await("selected chat after refresh", fmt.Sprintf(`JSON.parse(localStorage.getItem('osmia.selected')) === %q &&
      !document.querySelector('[data-view=%q]').hidden`, stream, view))
	var selection string
	p.Eval(`JSON.stringify({selected: JSON.parse(localStorage.getItem('osmia.selected')),
      view: document.querySelector('#main > .view:not([hidden])').dataset.view})`, &selection)
	state := ""
	if w.State != nil {
		state = *w.State
	}
	t.Logf("API kind=%q state=%q; browser %s", w.Kind, state, selection)
}
