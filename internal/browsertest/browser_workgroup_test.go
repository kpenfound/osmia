package browsertest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type workgroupPageFixture struct {
	s     *service.Service
	c     *service.Client
	repo  *trace.Repository
	group trace.Workgroup
	chief coreadapter.Scope
}

func newWorkgroupPageFixture(t *testing.T, edges bool) *workgroupPageFixture {
	t.Helper()
	ctx := context.Background()
	opts, cfg := servicetest.ConversationFixture(t, "workgroup-page-")
	withWeb(t, opts, "127.0.0.1:0")
	cfg.Project.Name = "Uploads repo"
	projectConfig, err := cfg.Root.ProjectConfig(project)
	must(t, err)
	must(t, config.WriteProjectConfig(projectConfig, cfg.Project))
	fixturekit.SeedPage(t, cfg)
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	g, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{stream, quiet}, demoStart, ownerActor, "fixture")
	must(t, err)
	if edges {
		spec := trace.ProjectSpec{Behavior: "Share the upload interface.", Graph: []trace.Edge{{From: stream, To: quiet, Type: trace.EdgeMergeOrder}}, Slices: []trace.Slice{{Workstream: stream, Spec: fixturekit.ValidSpec, Plan: `{"version":1,"units":[]}`}, {Workstream: quiet, Spec: fixturekit.ValidSpec, Plan: `{"version":1,"units":[]}`}}}
		must(t, trace.RecordProjectSpec(ctx, repo, g, spec, demoStart, ownerActor, "fixture"))
	}
	must(t, repo.Close())
	// The runner reconciles operations locally; no provider or model is used.
	opts.Threads = func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error) {
		return &completedRunner{}, nil
	}
	opts.Reconciliation.Ticks = make(chan time.Time)
	s, c := start(t, opts)
	live := service.InspectForTest(s).SoleRepository()
	_, err = live.EnsureChiefOfStaff(ctx, stream, demoStart, ownerActor)
	must(t, err)
	chief := claimTurn(t, live, t.TempDir(), stream, trace.ChiefOfStaff, trace.ChiefOfStaff, "group-status", demoStart)
	_, err = live.SetStatus(ctx, trace.ChiefOfStaff, chief, trace.StatusContent{Goal: "Ship shared uploads", Note: "The chief coordinates both repos.", Agents: []string{}}, time.Now().UTC(), func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	must(t, err)
	// A worker session, checks and review are real member-feed facts.
	fixturekit.QueueRoleTurn(t, live, stream, "group-worker", "mason", "upload", demoStart)
	_, err = live.ClaimTurn(ctx, stream, "group-worker", "group-worker-token", t.TempDir(), demoStart)
	must(t, err)
	fixturekit.FeedDocument(t, live, stream, "unit-upload-checks-1", "units/upload/checks-1.json", 1, demoStart, service.UnitCheckRun{Unit: "upload", Run: 1, Status: "passed", StartedAt: demoStart, FinishedAt: demoStart})
	fixturekit.FeedDocument(t, live, stream, "final-report", "final/report.json", 1, demoStart, api.FinalReport{Review: 1, Outcome: "reviewed", Reader: "reader", Turn: "review"})
	_, err = c.MoveUnit(ctx, stream, "upload", "contested", "Choose the upload approach.")
	must(t, err)
	return &workgroupPageFixture{s: s, c: c, repo: live, group: g, chief: chief}
}

func (f *workgroupPageFixture) question(t *testing.T, n int) api.InboxEntry {
	t.Helper()
	ctx := context.Background()
	id := fmt.Sprintf("group-question-%d", n)
	h := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: ownerActor, Cause: "fixture"}
	must(t, f.repo.CreateThread(ctx, trace.Agent{Header: h, Role: "mason", ThreadID: id}))
	q, err := f.repo.Ask(ctx, id, claimTurn(t, f.repo, t.TempDir(), stream, id, id, id+"-turn", demoStart), fmt.Sprintf("Shared decision %d?", n), time.Now().UTC())
	must(t, err)
	_, err = f.repo.EscalateQuestions(ctx, trace.ChiefOfStaff, f.chief, trace.EscalationRequest{Questions: []string{q.ID}, Rephrasing: fmt.Sprintf("Shared decision %d?", n), Blocked: "Both members", Recommendation: "Keep the shared interface."}, time.Now().UTC())
	must(t, err)
	entries := entriesOf(t, f.c, InboxEscalation)
	for _, e := range entries {
		if e.Question == fmt.Sprintf("Shared decision %d?", n) {
			return e
		}
	}
	t.Fatalf("shared question absent: %+v", entries)
	return api.InboxEntry{}
}

func openWorkgroup(p *page, id config.WorkgroupID) {
	p.T.Helper()
	p.Click(`[data-select-workgroup="` + string(id) + `"]`)
	p.Await("the selected workgroup", `location.hash === `+quote("#workgroup="+string(id))+` && !document.querySelector('[data-view=workgroup]').hidden`)
	p.AwaitText("#workgroup-content .feed", "The chief coordinates both repos.")
}

func TestBrowserWorkgroupViewConversationMembersAndDecisions(t *testing.T) {
	f := newWorkgroupPageFixture(t, true)
	shared := f.question(t, 1)
	p := openOutcomePage(t, f.s, `
  window.groupEventKind = null;
  const GroupEvents = window.EventSource;
  window.EventSource = class extends GroupEvents {
   addEventListener(kind,listener,options) {
    super.addEventListener(kind,event => {
     if (window.groupEventKind && ['workstream','conversation'].includes(kind) && window.groupEventKind !== kind) return;
     listener.call(this,event);
    },options);
   }
  };`)
	p.Eval(`window.groupDocument = 'original'`, nil)
	row := `[data-select-workgroup="` + string(f.group.ID) + `"]`
	p.AwaitText(row, "Ship shared uploads")
	p.AwaitText(row+" [data-field=needs]", "2")
	openWorkgroup(p, f.group.ID)
	p.AwaitText("#workgroup-content", "Waiting for you")
	p.AwaitText("#workgroup-content "+decisionCard(shared), "Shared decision 1?")
	p.AwaitText("#workgroup-content .decisions", "Choose the upload approach.")
	p.Await("repo-labelled decisions and both cards", `document.querySelectorAll('#workgroup-content [data-group-decision-repo]').length === 2 && [...document.querySelectorAll('#workgroup-content [data-group-decision-repo]')].every(e => e.textContent.includes('Uploads repo')) && document.querySelectorAll('#workgroup-content [data-member]').length === 2`)
	p.AwaitText("#workgroup-content [data-kind=action]", "Shared decision 1?")
	p.Await("the member action has a repo label", `document.querySelector('#workgroup-content [data-kind=action] [data-field=repo]') !== null`)
	p.Await("only shared entries and the dependency graph", `document.querySelector('#workgroup-content .workgroup-graph [data-edge]') !== null && !document.querySelector('#workgroup-content .feed [data-kind=transition], #workgroup-content .feed [data-kind=session], #workgroup-content .feed [data-kind=unit_checks], #workgroup-content .feed [data-kind=final_review]')`)
	p.Click(`[data-open-member="` + string(stream) + `"]`)
	p.Await("the member card opens its member", `!document.querySelector('[data-view=workstream]').hidden && location.hash === `+quote("#workstream="+string(stream)))
	p.Await("the member actually has detail events", `document.querySelector('#workstreams [data-kind=session]') !== null && document.querySelector('#workstreams [data-kind=transition]') !== null && document.querySelector('#workstreams [data-kind=unit_checks]') !== null && document.querySelector('#workstreams [data-kind=final_review]') !== null`)
	openWorkgroup(p, f.group.ID)
	p.Eval(`window.groupEventKind = "workstream"`, nil)
	_, statusErr := f.repo.SetStatus(context.Background(), trace.ChiefOfStaff, f.chief, trace.StatusContent{Goal: "Ship shared uploads", Note: "Both members now have a shared plan.", Agents: []string{}}, time.Now().UTC(), func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	must(t, statusErr)
	p.AwaitText("#workgroup-content [data-kind=status][data-latest=true]", "Both members now have a shared plan.")
	p.Eval(`window.groupEventKind = null`, nil)
	p.TypeInto("#workgroup-content .composer textarea", "Coordinate the whole group")
	p.Click("#workgroup-content .composer [type=submit]")
	p.AwaitText("#workgroup-content .composer .result", "Sent")
	p.AwaitText("#workgroup-content .feed", "Coordinate the whole group")
	// Finish the fixture chief's status turn, then the durable owner message.
	fixturekit.AnswerChief(t, f.repo, stream, "group-status", "Status recorded.")
	var q trace.QueuedTurn
	for i := 0; i < 20; i++ {
		var err error
		q, err = f.repo.ClaimTurn(context.Background(), stream, trace.ChiefOfStaff, fmt.Sprintf("group-reply-token-%d", i), t.TempDir(), time.Now().UTC())
		must(t, err)
		if q.Request.Prompt == "Coordinate the whole group" {
			break
		}
		fixturekit.AnswerChief(t, f.repo, stream, q.Request.TurnID, "Chief activity recorded.")
	}
	if q.Request.Prompt != "Coordinate the whole group" {
		t.Fatal("owner message did not follow the queued chief activity")
	}
	fixturekit.AnswerChief(t, f.repo, stream, q.Request.TurnID, "The group will coordinate.")
	p.AwaitText("#workgroup-content .feed", "The group will coordinate.")
	// These commits are separated by their rendered SSE result; no resync or
	// interval polling can coalesce the two member updates into one read.
	p.Eval(`window.groupEventKind = "conversation"`, nil)
	_, err := f.c.Send(context.Background(), quiet, "A message addressed to the other member")
	must(t, err)
	p.AwaitText("#workgroup-content .feed", "A message addressed to the other member")
	p.Eval(`window.groupEventKind = "workstream"`, nil)
	_, err = f.c.MoveWorkstream(context.Background(), quiet, "abandoned", "This member is cancelled.")
	must(t, err)
	p.AwaitText(`[data-member="`+string(quiet)+`"] [data-field=state]`, "cancelled")
	p.Await("the original page document survived events", `window.groupDocument === "original"`)
	p.Eval(`window.groupEventKind = "conversation"`, nil)
	p.Click(`[data-open-member="` + string(stream) + `"]`)
	_, err = f.c.Send(context.Background(), stream, "Shared update while away")
	must(t, err)
	p.Await("the workgroup row shows shared unread", `document.querySelector(`+quote(row+" [data-field=unread]")+`) !== null`)
	openWorkgroup(p, f.group.ID)
	p.AwaitText("#workgroup-content .feed", "Shared update while away")
	p.Await("opening the group clears its unread dot", `!document.querySelector(`+quote(row+" [data-field=unread]")+`)`)
	p.Eval(`window.groupEventKind = null`, nil)
	p.Click("#project-home")
	p.Click(`[data-group-inbox="` + string(f.group.ID) + `"]`)
	p.Await("project inbox opens the group", `!document.querySelector('[data-view=workgroup]').hidden`)
	p.Click("#project-home")
	p.Click(`[data-open-workgroup="` + string(f.group.ID) + `"]`)
	p.Await("project block opens the group", `!document.querySelector('[data-view=workgroup]').hidden`)
	// A workgroup hash is also a durable direct link.
	p.Run(chromedp.Reload())
	p.AwaitText("#workgroup-content .feed", "The group will coordinate.")
}

func TestBrowserWorkgroupDecisionIdentityAndLateOutcomes(t *testing.T) {
	f := newWorkgroupPageFixture(t, false)
	first, second := f.question(t, 1), f.question(t, 2)
	p := openOutcomePage(t, f.s)
	p.SelectWorkstream(stream)
	answer := func(e api.InboxEntry, text string) {
		card := decisionCard(e)
		p.AwaitText(card, e.Question)
		holdOutcome(p, e.Answer.Path)
		p.TypeInto(card+"textarea", text)
		p.Click(card + "[type=submit]")
		p.AwaitGone("the optimistically dismissed shared decision", card)
		assertOutcome(p, "#inbox-result", "pending", "Sending…")
	}
	answer(first, "Keep the first interface.")
	openWorkgroup(p, f.group.ID)
	assertOutcome(p, "#inbox-result", "", "")
	releaseOutcome(p, first.Answer.Path)
	settleWorkgroupAction(p)
	p.Await("the originating member receives the completion", `document.querySelector('#inbox-result').dataset.outcome === '' && typeof releaseOutcome[`+quote(first.Answer.Path)+`] === 'function'`)
	p.SelectWorkstream(stream)
	assertOutcome(p, "#inbox-result", "ok", fmt.Sprintf("Answered inbox entry %d.", first.Number))
	openWorkgroup(p, f.group.ID)
	answer(second, "Keep the second interface.")
	p.SelectWorkstream(quiet)
	assertOutcome(p, "#inbox-result", "", "")
	releaseOutcome(p, second.Answer.Path)
	settleWorkgroupAction(p)
	assertOutcome(p, "#inbox-result", "", "")
	openWorkgroup(p, f.group.ID)
	assertOutcome(p, "#inbox-result", "ok", fmt.Sprintf("Answered inbox entry %d.", second.Number))
	if got := entriesOf(t, f.c, InboxEscalation); len(got) != 0 {
		t.Fatalf("shared answers did not settle: %+v", got)
	}
	p.Await("one dispatch per recorded shared answer", `outcomeRequests[`+quote(first.Answer.Path)+`] === 1 && outcomeRequests[`+quote(second.Answer.Path)+`] === 1`)
	p.Await("no graph without edges", `!document.querySelector('#workgroup-content .workgroup-graph')`)
	_, err := f.repo.ProposeCharter(context.Background(), trace.ChiefOfStaff, f.chief, first.Asked[0].ID, "Keep upload interfaces compatible.", func(string) int { return 2 }, time.Now().UTC())
	must(t, err)
	p.AwaitText("#workgroup-content [data-proposal]", "Keep upload interfaces compatible.")
	p.AwaitText("#workgroup-content [data-proposal] [data-group-decision-repo]", "Uploads repo")
	p.AwaitText(`[data-select-workgroup="`+string(f.group.ID)+`"] [data-field=needs]`, "2")
	p.Click("#workgroup-content [data-proposal] [data-field=ratify]")
	assertOutcome(p, "#inbox-result", "ok", "Charter decision recorded.")
	p.AwaitGone("the ratified charter decision", "#workgroup-content [data-proposal]")
}

func TestBrowserWorkgroupPhoneLayout(t *testing.T) {
	f := newWorkgroupPageFixture(t, false)
	longGoal := strings.Repeat("UnbrokenGoal", 20)
	_, err := f.repo.SetStatus(context.Background(), trace.ChiefOfStaff, f.chief, trace.StatusContent{Goal: longGoal, Note: "The chief coordinates both repos.", Agents: []string{}}, time.Now().UTC(), func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	must(t, err)
	p := openOutcomePage(t, f.s)
	openWorkgroup(p, f.group.ID)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile))
	p.Await("stacked cards above the conversation and a pinned composer without overflow", `(() => {
  const cards = [...document.querySelectorAll('#workgroup-content [data-member]')].map(e => e.getBoundingClientRect());
  const feed = document.querySelector('#workgroup-content .feed').getBoundingClientRect();
  const composer = document.querySelector('#workgroup-content .composer');
  const rect = composer.getBoundingClientRect();
  const main = document.querySelector('#main');
  return cards.length === 2 && cards[0].bottom <= cards[1].top && cards[1].bottom <= feed.top && getComputedStyle(composer).position === 'sticky' && getComputedStyle(composer).bottom === '0px' && rect.bottom <= innerHeight && rect.top > 0 && document.documentElement.scrollWidth <= innerWidth && main.scrollWidth <= main.clientWidth;
 })()`)
}

func settleWorkgroupAction(p *page) {
	p.T.Helper()
	p.Run(chromedp.Evaluate(`new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))`, nil, func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams { return p.WithAwaitPromise(true) }))
}

func TestBrowserWorkgroupArchivedCardsAndDisabledRow(t *testing.T) {
	ctx := context.Background()
	opts, repo := sidebarFixture(t)
	retired := config.WorkstreamID("w_000000000000000000000000000000a3")
	otherRetired := config.WorkstreamID("w_000000000000000000000000000000a4")
	for _, id := range []config.WorkstreamID{stream, quiet, retired, otherRetired} {
		must(t, repo.CreateWorkstream(ctx, id, demoStart, ownerActor))
	}
	active, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{stream, quiet}, demoStart, ownerActor, "fixture")
	must(t, err)
	archived, err := trace.CreateWorkgroup(ctx, repo, []config.WorkstreamID{retired, otherRetired}, demoStart, ownerActor, "fixture")
	must(t, err)
	fixturekit.SetStatusGoal(t, repo, stream, "Continue the live group", demoStart)
	root, err := opts.Config.Resolve()
	must(t, err)
	archives := []runtime.Archive{}
	for _, id := range []config.WorkstreamID{quiet, retired, otherRetired} {
		group := active.ID
		if id != quiet {
			group = archived.ID
		}
		archives = append(archives, runtime.Archive{Project: project, Workstream: id, Workgroup: group, Title: "Archived shared goal", State: "abandoned", Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: demoStart, CleanedAt: demoStart.Add(time.Minute)})
		_, err = repo.MarkWorkstreamTraceForDeletion(id)
		must(t, err)
		must(t, trace.FinishWorkstreamTraceDeletion(root, project, id))
	}
	must(t, repo.Close())
	data, err := json.Marshal(runtime.State{Version: runtime.Version, Archived: archives})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(opts.Config.Root, "runtime.json"), data, 0600))
	opts.Threads = func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error) {
		return &completedRunner{}, nil
	}
	s, _ := start(t, opts)
	p := openOutcomePage(t, s)
	p.Await("the archived workgroup is disabled", `document.querySelector('#archived [data-select-workgroup="`+string(archived.ID)+`"]')?.disabled === true`)
	p.AwaitText(`#workstream-list [data-select-workgroup="`+string(active.ID)+`"]`, "Continue the live group")
	p.Click(`#workstream-list [data-select-workgroup="` + string(active.ID) + `"]`)
	p.AwaitText(`[data-member="`+string(quiet)+`"] [data-field=archived]`, "Archived")
	p.AwaitText(`[data-member="`+string(quiet)+`"] [data-field=state]`, "cancelled")
	p.Await("the retired member cannot open deleted history", `document.querySelector('[data-open-member="`+string(quiet)+`" ]').disabled`)
	for _, a := range archives {
		if _, err := os.Lstat(filepath.Join(opts.Config.Root, "workstreams", string(project), string(a.Workstream))); !os.IsNotExist(err) {
			t.Fatalf("archived history was recreated: %v", err)
		}
	}
}

func TestBrowserWorkgroupSharedAnswerPreservesRecordedTarget(t *testing.T) {
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	withWeb(t, f.opts, "127.0.0.1:0")
	members := []trace.CrossRepoMember{}
	for _, id := range []config.ProjectID{project, otherProject} {
		repo, err := trace.Open(f.cfg.Root, f.cfg.For(id).Project)
		must(t, err)
		members = append(members, trace.CrossRepoMember{Project: repo, Workstream: f.streams[id]})
	}
	chief, g, err := trace.CreateWorkgroupTrace(ctx, f.cfg.Root, members, demoStart, ownerActor, "fixture")
	must(t, err)
	for _, m := range members {
		must(t, m.Project.JoinCrossRepoWorkgroup(ctx, m.Workstream, g.ID, demoStart, ownerActor, "fixture"))
	}
	host := g.Members[0]
	_, err = chief.EnsureChiefOfStaff(ctx, host, demoStart, ownerActor)
	must(t, err)
	claim := func(repo *trace.Repository, ws config.WorkstreamID, agent, role, turn string) coreadapter.Scope {
		t.Helper()
		if agent != trace.ChiefOfStaff {
			must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: agent, Revision: 1, Project: repo.Project(), Workstream: ws, At: demoStart, Actor: ownerActor, Cause: "fixture"}, Role: role, ThreadID: agent}))
		}
		h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request-" + turn, Revision: 1, Project: repo.Project(), Workstream: ws, At: demoStart, Actor: ownerActor, Cause: "fixture"}
		_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: agent, ThreadID: agent, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Coordinate the shared interface"})
		must(t, err)
		_, err = repo.ClaimTurn(ctx, ws, agent, "token-"+turn, t.TempDir(), demoStart)
		must(t, err)
		return coreadapter.Scope{Project: string(repo.Project()), Workstream: string(ws), Role: role, Thread: agent, Turn: turn}
	}
	cs := claim(chief, host, trace.ChiefOfStaff, trace.ChiefOfStaff, "shared-question-chief")
	_, err = chief.SetStatus(ctx, trace.ChiefOfStaff, cs, trace.StatusContent{Goal: "Coordinate two repositories", Note: "One chief owns the shared question.", Agents: []string{}}, demoStart, func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
	must(t, err)
	qs := []string{}
	for i, m := range members {
		agent := fmt.Sprintf("shared-worker-%d", i)
		scope := claim(m.Project, m.Workstream, agent, "mason", agent+"-turn")
		q, err := m.Project.Ask(ctx, agent, scope, "May the shared interface change?", demoStart)
		must(t, err)
		qs = append(qs, string(m.Workstream)+":"+q.ID)
	}
	_, err = chief.EscalateQuestions(ctx, trace.ChiefOfStaff, cs, trace.EscalationRequest{Questions: qs, Rephrasing: "May the shared interface change?", Blocked: "Both repositories", Recommendation: "Keep the interface stable."}, demoStart)
	must(t, err)
	synthetic := chief.Project()
	must(t, chief.Close())
	for _, m := range members {
		must(t, m.Project.Close())
	}
	f.opts.Threads = func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error) {
		return &completedRunner{}, nil
	}
	f.opts.Reconciliation.Ticks = make(chan time.Time)
	s, c := start(t, f.opts)
	entries := entriesOf(t, c, InboxEscalation)
	if len(entries) != 1 || entries[0].Workgroup != g.ID || entries[0].Project != synthetic {
		t.Fatalf("shared decision identity: %+v", entries)
	}
	e := entries[0]
	p := openOutcomePage(t, s)
	p.AwaitText(`[data-select-workgroup="`+string(g.ID)+`"]`, "Coordinate two repositories")
	p.Click(`[data-select-workgroup="` + string(g.ID) + `"]`)
	p.AwaitText("#workgroup-content "+decisionCard(e), e.Question)
	p.Await("shared decision carries repo labels", `document.querySelector('#workgroup-content [data-group-decision-repo]').textContent.startsWith('Workgroup · ')`)
	p.Eval(`(() => { const prior = window.fetch; window.fetch = (url,init) => { if (init?.method === 'POST') window.workgroupAnswer = {url,method:init.method,body:JSON.parse(init.body)}; return prior(url,init); }; })()`, nil)
	holdOutcome(p, e.Answer.Path)
	p.TypeInto(decisionCard(e)+"textarea", "Keep both repositories compatible.")
	p.Click(decisionCard(e) + "[type=submit]")
	p.AwaitGone("the shared decision dismisses by its original key", decisionCard(e))
	var actual struct {
		URL, Method string
		Body        map[string]any
	}
	p.Eval(`window.workgroupAnswer`, &actual)
	expected := make(map[string]any)
	for k, v := range e.Answer.Body {
		expected[k] = v
	}
	expected["text"] = "Keep both repositories compatible."
	got, _ := json.Marshal(actual.Body)
	want, _ := json.Marshal(expected)
	if actual.URL != e.Answer.Path || actual.Method != e.Answer.Method || string(got) != string(want) {
		t.Fatalf("answer target changed: %+v, want %s %s %s", actual, e.Answer.Method, e.Answer.Path, want)
	}
	releaseOutcome(p, e.Answer.Path)
	assertOutcome(p, "#inbox-result", "ok", fmt.Sprintf("Answered inbox entry %d.", e.Number))
	if remaining := entriesOf(t, c, InboxEscalation); len(remaining) != 0 {
		t.Fatalf("shared answer did not settle: %+v", remaining)
	}
}
