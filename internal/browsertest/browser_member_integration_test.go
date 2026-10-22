package browsertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// awaitOwnerTurn reads the production runner's completed durable turn, including
// the routing note originally recorded by the conversation endpoint.
func (f *workgroupPageFixture) awaitOwnerTurn(t *testing.T, text string, member config.WorkstreamID) {
	t.Helper()
	deadline := time.Now().Add(browserTimeout)
	for {
		thread, err := f.repo.ChiefOfStaffThread(stream)
		must(t, err)
		for _, q := range thread.Turns {
			if q.Request.Prompt != text || q.CompletedAt.IsZero() {
				continue
			}
			note := "The owner addressed workstream "
			if member == "" && strings.Contains(q.Request.SystemPrompt, note) {
				t.Fatalf("whole-group message has member note: %s", q.Request.SystemPrompt)
			}
			if member != "" && !strings.Contains(q.Request.SystemPrompt, note+string(member)) {
				t.Fatalf("member message lost note: %s", q.Request.SystemPrompt)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner turn %q did not complete", text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runChief uses the production dispatcher, grants and thread runner with a fake
// execution backend; only its scripted replies replace model execution.
func (f *workgroupPageFixture) runChief(t *testing.T) {
	t.Helper()
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Reason: "Keep worker activity stable during chief conversation", Source: runtime.PauseOwner})
	f.c.Close()
	must(t, f.s.Close())
	path := filepath.Join(f.opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(top, []byte("\n[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")...), 0600))
	sessions := &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &fixturekit.DemoEngine{Sessions: sessions, Turns: map[string]fixturekit.DemoTurn{}}
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	replies := map[string]string{
		"Question about this repo":     "This repo can use the shared contract.",
		"Question for the whole group": "Both repos will coordinate their changes.",
		"A new shared update":          "A shared answer while the owner is away.",
	}
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		reply := "Chief activity recorded."
		for text, answer := range replies {
			if strings.HasSuffix(strings.TrimSpace(req.Prompt), text) {
				got, err := fixturekit.CallTool(ctx, tools, "set_status", map[string]any{"goal": "Ship shared uploads", "note": "Chief status after " + text, "attention": "Choose the shared interface and upload approach.", "agents": []string{}})
				if err != nil || !strings.Contains(got, `"stored":true`) {
					return nil, fmt.Errorf("set status: %s %v", got, err)
				}
				reply = answer
			}
		}
		return &agent.Result{ClaudeID: "chief-" + req.Name, ResultText: reply}, nil
	}
	f.opts.Reconciliation.Now = time.Now
	f.opts = service.EnforceThreadsForTest(f.opts, service.Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}}})
	f.s, f.c = start(t, f.opts)
	f.repo = service.InspectForTest(f.s).SoleRepository()
}

func TestBrowserMemberConversationRoutingAcrossRestart(t *testing.T) {
	f := newWorkgroupPageFixture(t, false)
	shared := f.question(t, 1)
	fixturekit.AnswerChief(t, f.repo, stream, "group-status", "Status recorded.")
	f.runChief(t)
	p := openOutcomePage(t, f.s, `
  window.memberPosts = [];
  const postFetch = window.fetch.bind(window);
  window.fetch = (url, init) => {
   if (init?.method === 'POST') window.memberPosts.push(url);
   return postFetch(url, init);
  };`)
	p.SelectWorkstream(quiet)
	p.AwaitGone("the member has no shared decision", "#workstreams "+decisionCard(shared))
	p.Click("#workstream-head [data-ask-chief]")
	p.Await("the repo chip and composer are focused", `location.hash === `+quote("#workgroup="+string(f.group.ID))+` && document.activeElement === document.querySelector('#workgroup-content .composer textarea')`)
	p.AwaitText("#workgroup-content [data-field=about]", "About: Uploads repo")
	p.TypeInto("#workgroup-content .composer textarea", "Question about this repo")
	p.Click("#workgroup-content .composer [type=submit]")
	p.AwaitText("#workgroup-content .composer .result", "Sent")
	f.awaitOwnerTurn(t, "Question about this repo", quiet)
	p.AwaitText("#workgroup-content .feed", "This repo can use the shared contract.")
	p.Click("#workgroup-content [aria-label='Send to the whole workgroup']")
	p.AwaitGone("the cleared repo chip", "#workgroup-content [data-field=about]")
	p.TypeInto("#workgroup-content .composer textarea", "Question for the whole group")
	p.Click("#workgroup-content .composer [type=submit]")
	p.AwaitText("#workgroup-content .feed", "Question for the whole group")
	f.awaitOwnerTurn(t, "Question for the whole group", "")
	p.AwaitText("#workgroup-content .feed", "Both repos will coordinate their changes.")
	p.Await("both production send paths were used once", `memberPosts.filter(x => x === `+quote("/v1/conversation/"+string(quiet))+`).length === 1 && memberPosts.filter(x => x === `+quote("/v1/workgroups/"+string(f.group.ID)+"/conversation")+`).length === 1`)
	// Shared status changes and messages affect only the group's unread state.
	p.SelectWorkstream(quiet)
	p.AwaitText("#workstream-head [data-field=chief-status]", "Chief status after Question for the whole group")
	p.Click("#project-home")
	_, err := f.c.Send(context.Background(), quiet, "A new shared update")
	must(t, err)
	f.awaitOwnerTurn(t, "A new shared update", quiet)
	p.Await("only the workgroup is unread", `document.querySelector('[data-select-workgroup="`+string(f.group.ID)+`"] [data-field=unread]') && !document.querySelector('[data-select="`+string(quiet)+`"] [data-field=unread]')`)
	for _, id := range []config.WorkstreamID{stream, quiet} {
		var feed api.FeedResponse
		err := f.c.Do(context.Background(), "GET", "/v1/feed/"+string(id), nil, &feed)
		must(t, err)
		for _, e := range feed.Entries {
			if e.Kind == "message" || e.Kind == "response" || e.Kind == "action" || e.Kind == "status" || (e.Session != nil && e.Session.Role == trace.ChiefOfStaff) {
				t.Fatalf("shared entry leaked into %s: %+v", id, e)
			}
		}
	}
	f.c.Close()
	must(t, f.s.Close())
	s, c := start(t, f.opts)
	f.s, f.c = s, c
	f.repo = service.InspectForTest(s).SoleRepository()
	p.Run(chromedp.Navigate("http://" + s.WebAddr() + "/#workgroup=" + string(f.group.ID)))
	p.AwaitText("#workgroup-content .feed", "Question about this repo")
	p.AwaitText("#workgroup-content .feed", "This repo can use the shared contract.")
	p.AwaitText("#workgroup-content .feed", "Question for the whole group")
	p.AwaitText("#workgroup-content .feed", "Both repos will coordinate their changes.")
	p.AwaitText("#workgroup-content "+decisionCard(shared), shared.Question)
	p.Await("both cards and member decision survive restart", `document.querySelectorAll('#workgroup-content [data-member]').length === 2 && document.querySelector('#workgroup-content [data-kind=contested]') !== null`)
}

func TestBrowserOpenSingleMemberGainsFollowup(t *testing.T) {
	ctx := context.Background()
	f := newMemberPageFixture(t, false, false)
	shared := f.question(t, 1)
	_, err := f.c.Send(ctx, stream, "The conversation before another repo joins")
	must(t, err)
	p := openOutcomePage(t, f.s)
	p.SelectWorkstream(stream)
	p.AwaitText("#workstreams .feed", "The conversation before another repo joins")
	p.AwaitText("#workstreams "+decisionCard(shared), shared.Question)
	p.Await("single-member chat and no group row", `document.querySelector('#workstreams .composer textarea') && !document.querySelector('[data-select-workgroup]')`)
	p.TypeInto("#workstreams .composer textarea", "Unsent single-member draft")
	p.Eval(`window.membershipDocument = 'same document'`, nil)
	// This is the production membership path used when an approved amendment
	// adds a follow-up. It allocates and joins the member through the controller,
	// preserving the existing group's authority and conversation.
	sl := trace.Slice{Workstream: stream, Spec: fixturekit.ValidSpec, Plan: fixturekit.ValidPlan}
	change := lifecycle.WorkgroupChange{ID: "browser-followup", Host: stream, Workgroup: f.group.ID,
		State: "presented", Request: lifecycle.ChangeRequest{Kind: "amend", Scope: "workgroup", Change: "Add the approved follow-up"},
		After: trace.ProjectSpec{Behavior: "Keep the upload contract", Slices: []trace.Slice{sl}}}
	_, err = service.InspectForTest(f.s).LifecycleController().CreateChangeFollowup(ctx, f.repo, &f.group, f.repo,
		amendment.StreamImpact{Workstream: stream, Action: amendment.FollowUp}, &change)
	must(t, err)
	added := change.Followups[stream]
	if added == "" {
		t.Fatal("production controller did not allocate the follow-up")
	}
	p.Await("the same open member is trimmed after the join", `window.membershipDocument === 'same document' && document.querySelector('#workstream-head').dataset.workstream === `+quote(string(stream))+` && document.querySelector('#workstream-head [data-ask-chief]') && !document.querySelector('#workstreams .composer, #workstreams [data-kind=message], #workstreams [data-kind=response], #workstreams [data-kind=status], #workstreams [data-kind=action], #workstreams [data-kind=escalation]')`)
	p.Await("the group row appears through events", `document.querySelector('[data-select-workgroup="`+string(f.group.ID)+`"]') !== null`)
	openWorkgroup(p, f.group.ID)
	p.AwaitText("#workgroup-content .feed", "The conversation before another repo joins")
	p.AwaitText("#workgroup-content "+decisionCard(shared), shared.Question)
	p.Await("both member cards are present", `document.querySelector('#workgroup-content [data-member="`+string(stream)+`"]') && document.querySelector('#workgroup-content [data-member="`+string(added)+`"]')`)
	p.Await("the new group composer did not inherit the member draft", `document.querySelector('#workgroup-content .composer textarea').value === ''`)
}
