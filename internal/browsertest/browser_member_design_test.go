package browsertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBrowserDesignStartFollowsMultiMemberWorkgroup(t *testing.T) {
	ctx := context.Background()
	opts, clone := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	must(t, err)
	top = append(top, []byte("\n[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[roles.architect]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")...)
	must(t, os.WriteFile(path, top, 0600))
	second := filepath.Join(filepath.Dir(clone), "client")
	for _, dir := range []string{clone, second} {
		must(t, os.MkdirAll(dir, 0700))
		fixturekit.DemoGit(t, filepath.Dir(clone), "-C", dir, "init", "-q")
		must(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("Upload contract\n"), 0600))
		fixturekit.DemoGit(t, filepath.Dir(clone), "-C", dir, "add", "README.md")
		fixturekit.DemoGit(t, filepath.Dir(clone), "-C", dir, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	}
	sessions := &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &fixturekit.DemoEngine{Sessions: sessions, Turns: map[string]fixturekit.DemoTurn{}}
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	var repos []string
	engine.Turns["select-repos-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		got, err := fixturekit.CallTool(ctx, tools, intake.AffectedReposTool, map[string]any{"repos": repos})
		if err != nil || !strings.Contains(got, `"recorded":true`) {
			return nil, fmt.Errorf("select repos: %s %v", got, err)
		}
		return &agent.Result{ClaudeID: "selection", ResultText: "Both repositories are affected."}, nil
	}
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if name == intake.DraftBriefToolName {
				_, err = fixturekit.CallTool(ctx, tools, name, map[string]any{"content": intakefixture.ProductionBrief})
				if err != nil {
					return nil, err
				}
			}
		}
		return &agent.Result{ClaudeID: "design", ResultText: "The design conversation continues in the workgroup."}, nil
	}
	hosts := &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}}
	opts.Architect = &service.Architect{Engine: engine, Hosts: hosts}
	opts = service.EnforceThreadsForTest(opts, service.Enforcement{Engine: engine, Hosts: hosts})
	s, c := start(t, opts)
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	must(t, err)
	other, err := c.AddRepo(ctx, api.RepoAddRequest{Project: added.Project.ID, Upstream: "owner/client", Clone: second})
	must(t, err)
	repos = []string{string(added.Project.ID)}
	for _, repo := range other.Project.Repos {
		if repo.Clone == second {
			repos = append(repos, string(repo.ID))
		}
	}
	if len(repos) != 2 {
		t.Fatalf("second repo was not registered: %+v", other)
	}
	writeCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	p := openOutcomePage(t, s)
	p.Click("#sidebar-new-design")
	p.Choose("#design-add-form [name=project]", string(added.Project.ID))
	p.Click("#design-add-form [type=submit]")
	p.Await("the new design view", `!document.querySelector('[data-view=design]').hidden`)
	p.TypeInto("#design-form [name=text]", "Build the upload contract in both repos")
	p.Click("#design-form [type=submit]")
	p.AwaitText("#design-feed", "The design conversation continues in the workgroup.")
	designs, err := c.Designs(ctx, added.Project.ID)
	must(t, err)
	source := designs.Designs[0].Workstream
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		repo, err := service.InspectForTest(s).Repository(added.Project.ID)
		if err != nil {
			t.Log(err)
			return
		}
		th, err := repo.Thread(source, intake.ArchitectAgent)
		t.Logf("architect thread error=%v", err)
		for _, q := range th.Turns {
			if q.Response != nil {
				t.Logf("architect turn=%s failure=%s", q.Request.TurnID, q.Response.Failure)
			}
		}
	})
	p.Click("#design-start")
	p.Await("design start follows the multi-member workgroup", `location.hash.startsWith('#workgroup=') && !document.querySelector('[data-view=workgroup]').hidden && document.querySelectorAll('#workgroup-content [data-member]').length === 2`)
	p.AwaitText("#workgroup-content .feed", "Build the upload contract in both repos")
	p.AwaitText("#workgroup-content .feed", "The design conversation continues in the workgroup.")
	// Reopening the original design URL follows its adopted conversation too.
	p.Run(chromedp.Navigate("http://"+s.WebAddr()+"/#workstream="+string(source)), chromedp.Reload())
	p.Await("the original design link follows to the group", `location.hash.startsWith('#workgroup=') && !document.querySelector('[data-view=workgroup]').hidden`)
}
