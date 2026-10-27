package intake

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const charterProjectB config.ProjectID = "p_00000000000000000000000000000022"
const charterMemberB config.WorkstreamID = "w_00000000000000000000000000000022"

type crossCharterFixture struct {
	cfg         *config.Config
	a, b, group *trace.Repository
	workgroup   trace.Workgroup
	home        string
}

func newCrossCharterFixture(t *testing.T) *crossCharterFixture {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "charters-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.Load(fixturekit.ConfigAt(t, home))
	fixturekit.Must(t, err)
	owner := OwnerActor
	a, err := trace.CreateSeeded(ctx, cfg.Root, cfg.Project, fixturekit.DemoStart, owner, []byte(`{"version":1,"entities":[{"id":"internal.trace","name":"Trace","paths":["internal/trace"]}]}`))
	fixturekit.Must(t, err)
	t.Cleanup(func() { a.Close() })
	bProject := cfg.Project
	bProject.ID, bProject.Name = charterProjectB, "B"
	bProject.Clone = filepath.Join(home, "B")
	b, err := trace.Create(ctx, cfg.Root, bProject, fixturekit.DemoStart, owner)
	fixturekit.Must(t, err)
	t.Cleanup(func() { b.Close() })
	fixturekit.RecordCharter(t, a, "1. A requires a durable journal.\n\n2. A alone has this rule.\n")
	fixturekit.RecordCharter(t, b, "1. B requires an ephemeral journal.\n\n3. B alone has this rule.\n")
	fixturekit.Must(t, a.CreateWorkstream(ctx, fixturekit.Stream, fixturekit.DemoStart, owner))
	fixturekit.Must(t, b.CreateWorkstream(ctx, charterMemberB, fixturekit.DemoStart, owner))
	group, g, err := trace.CreateWorkgroupTrace(ctx, cfg.Root, []trace.CrossRepoMember{{Workstream: fixturekit.Stream, Project: a}, {Workstream: charterMemberB, Project: b}}, fixturekit.DemoStart, owner, "test")
	fixturekit.Must(t, err)
	t.Cleanup(func() { group.Close() })
	for _, m := range []trace.CrossRepoMember{{Workstream: fixturekit.Stream, Project: a}, {Workstream: charterMemberB, Project: b}} {
		fixturekit.Must(t, m.Project.RecordCrossRepoWorkgroup(ctx, m.Workstream, g.ID, fixturekit.DemoStart, owner, "test"))
	}
	return &crossCharterFixture{cfg, a, b, group, g, home}
}

func (f *crossCharterFixture) askConflict(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	h := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: "mason", Revision: 1, Project: f.a.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: OwnerActor, Cause: "test"}
	fixturekit.Must(t, f.a.CreateThread(ctx, trace.Agent{Header: h, Role: "mason", ThreadID: "mason"}))
	scope := fixturekit.ClaimTurn(t, f.a, f.home, fixturekit.Stream, "mason", "mason", "build", fixturekit.DemoStart)
	tools, err := questions.Tools(f.a, "mason", scope, func() time.Time { return fixturekit.DemoStart })
	fixturekit.Must(t, err)
	for _, citations := range [][]string{{"charter#1", "p_missing/charter#1"}, {"charter#1", string(charterProjectB) + "/charter#2"}, {"charter#1", "charter#2"}} {
		raw, _ := json.Marshal(map[string]any{"question": "Rules conflict.", "charter_conflict": citations})
		out, err := tools[0].Handle(ctx, raw)
		fixturekit.Must(t, err)
		if !strings.Contains(string(out), `"recorded":false`) {
			t.Fatalf("accepted invalid conflict: %s", out)
		}
	}
	raw, _ := json.Marshal(map[string]any{"question": "A requires durability but B requires ephemeral state; which rule applies?", "charter_conflict": []string{"charter#1", string(charterProjectB) + "/charter#1"}})
	out, err := tools[0].Handle(ctx, raw)
	fixturekit.Must(t, err)
	if !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("ask: %s", out)
	}
	return "1"
}

func TestCrossProjectCharterValidationAndContext(t *testing.T) {
	f := newCrossCharterFixture(t)
	ctx := context.Background()
	for _, repo := range []*trace.Repository{f.a, f.group} {
		own, rule, err := repo.ResolveCharter(ctx, fixturekit.Stream, "charter#1")
		fixturekit.Must(t, err)
		if own.Project != f.a.Project() || !strings.Contains(rule.Text, "A requires") {
			t.Fatalf("local charter: %+v %+v", own, rule)
		}
		other, rule, err := repo.ResolveCharter(ctx, fixturekit.Stream, string(charterProjectB)+"/charter#1")
		fixturekit.Must(t, err)
		if other.Project != charterProjectB || !strings.Contains(rule.Text, "B requires") {
			t.Fatalf("qualified charter: %+v %+v", other, rule)
		}
	}
	draft := &Drafter{Repository: f.a}
	spec := "# Journal\n\n## Acceptance criteria\n\n1. Preserve the journal.\n"
	graph := `{"version":2,"units":[{"id":"journal","task":"Preserve the journal.","acceptance":["The journal persists."],"criteria":["spec#1"],"footprint":["internal.trace"],"constraints":["charter#2", "` + string(charterProjectB) + `/charter#3"]}]}`
	problems, err := draft.Validate(trace.Document{Header: trace.Header{Workstream: fixturekit.Stream}, Content: spec}, trace.Document{Content: graph})
	fixturekit.Must(t, err)
	if len(problems) > 0 {
		t.Fatalf("valid member plan rejected: %v", problems)
	}
	for _, bad := range []string{"charter#3", string(charterProjectB) + "/charter#2", "p_missing/charter#1"} {
		problems, err := draft.Validate(trace.Document{Header: trace.Header{Workstream: fixturekit.Stream}, Content: spec}, trace.Document{Content: strings.Replace(graph, "charter#2", bad, 1)})
		fixturekit.Must(t, err)
		if !strings.Contains(strings.Join(problems, ";"), "citation "+bad) {
			t.Fatalf("plan accepted %s: %v", bad, problems)
		}
	}
	valid := "Constraints: charter#2 and " + string(charterProjectB) + "/charter#3"
	if p := plan.CharterProblems(ctx, f.a, fixturekit.Stream, valid); len(p) > 0 {
		t.Fatal(p)
	}
	for _, bad := range []string{"charter#3", string(charterProjectB) + "/charter#2", "p_missing/charter#1"} {
		if p := plan.CharterProblems(ctx, f.a, fixturekit.Stream, bad); len(p) != 1 {
			t.Fatalf("accepted %s: %v", bad, p)
		}
	}
	provider := bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return f.a, nil }}
	for _, role := range []string{"architect", "committee", trace.ChiefOfStaff} {
		b, err := provider.Assemble(ctx, f.a.Project(), bundle.Scope{Role: role, Workstream: fixturekit.Stream})
		fixturekit.Must(t, err)
		for _, required := range []string{string(f.a.Project()) + "/charter#1: A requires", string(charterProjectB) + "/charter#1: B requires", "workgroup spec and contracts must satisfy every charter"} {
			if !strings.Contains(b.Brief(), required) {
				t.Fatalf("%s context missing %s", role, required)
			}
		}
	}
	// An authority reopened without bound member handles still reads every charter.
	fixturekit.Must(t, f.group.Close())
	reopened, err := trace.OpenWorkgroupAuthority(f.cfg.Root, f.workgroup.ID)
	fixturekit.Must(t, err)
	defer reopened.Close()
	charters, err := reopened.WorkgroupCharters(ctx, f.workgroup.Members[0])
	fixturekit.Must(t, err)
	if len(charters) != 2 {
		t.Fatalf("reopened authority charters: %+v", charters)
	}
}

func TestCrossProjectCharterConflictAndAmendment(t *testing.T) {
	f := newCrossCharterFixture(t)
	ctx := context.Background()
	q := f.askConflict(t)
	inbox, err := f.group.Inbox()
	fixturekit.Must(t, err)
	if len(inbox) != 1 || inbox[0].State != trace.QuestionEscalated {
		t.Fatalf("owner inbox: %+v", inbox)
	}
	host := f.workgroup.Members[0]
	_, err = f.group.EnsureChiefOfStaff(ctx, host, fixturekit.DemoStart, OwnerActor)
	fixturekit.Must(t, err)
	chief := claimCharterChief(t, f.group, host)
	if _, err := f.group.AnswerQuestion(ctx, trace.ChiefOfStaff, chief, string(fixturekit.Stream)+":"+q, "Dismiss it.", []string{"charter#1"}, fixturekit.DemoStart); err == nil || !strings.Contains(err.Error(), "only the owner's ruling") {
		t.Fatalf("chief answered conflict: %v", err)
	}
	_, err = f.group.Rule(ctx, inbox[0].Number, "B may persist the coordination journal.", OwnerActor, fixturekit.DemoStart)
	fixturekit.Must(t, err)
	tools, err := questions.Tools(f.group, trace.ChiefOfStaff, chief, func() time.Time { return fixturekit.DemoStart })
	fixturekit.Must(t, err)
	for _, tool := range tools {
		if tool.Name == questions.AnswerTool {
			raw, _ := json.Marshal(map[string]any{"question": string(fixturekit.Stream) + ":" + q, "text": "Ignore B.", "citations": []string{"charter#1"}})
			out, err := tool.Handle(ctx, raw)
			fixturekit.Must(t, err)
			if !strings.Contains(string(out), `"recorded":false`) {
				t.Fatalf("chief answered charter conflict: %s", out)
			}
		}
		if tool.Name != questions.ProposeCharterTool {
			continue
		}
		raw, _ := json.Marshal(map[string]string{"question": string(fixturekit.Stream) + ":" + q, "rule": "The coordination journal may be durable.", "amends": string(charterProjectB) + "/charter#1"})
		out, err := tool.Handle(ctx, raw)
		fixturekit.Must(t, err)
		if !strings.Contains(string(out), `"recorded":true`) {
			t.Fatalf("propose: %s", out)
		}
	}
	proposalsA, err := f.a.CharterProposals()
	fixturekit.Must(t, err)
	proposalsB, err := f.b.CharterProposals()
	fixturekit.Must(t, err)
	if len(proposalsA) != 0 || len(proposalsB) != 1 {
		t.Fatalf("routed proposals: A=%+v B=%+v", proposalsA, proposalsB)
	}
	for _, tool := range tools {
		if tool.Name != questions.ProposeCharterTool {
			continue
		}
		raw, _ := json.Marshal(map[string]string{"question": string(fixturekit.Stream) + ":" + q, "rule": "Another standing rule.", "amends": "charter#1"})
		out, err := tool.Handle(ctx, raw)
		fixturekit.Must(t, err)
		if !strings.Contains(string(out), `"recorded":false`) || !strings.Contains(string(out), "already has a charter proposal") {
			t.Fatalf("duplicated routed ruling: %s", out)
		}
	}
	proposal := proposalsB[0]
	if proposal.Proposal.SourceProject != f.a.Project() || proposal.Proposal.SourceWorkstream != fixturekit.Stream || proposal.Proposal.SourceQuestion != q {
		t.Fatalf("origin: %+v", proposal)
	}
	beforeA, err := f.a.Charter(ctx)
	fixturekit.Must(t, err)
	beforeB, err := f.b.Charter(ctx)
	fixturekit.Must(t, err)
	controller := New(Dependencies{Now: func() time.Time { return fixturekit.DemoStart }, Runtimes: func() (*config.Config, []*Project) {
		return f.cfg, []*Project{{ID: f.a.Project(), Repository: f.a}, {ID: f.b.Project(), Repository: f.b}}
	}})
	// Decisions addressed through the original member still reach B after reopening.
	fixturekit.Must(t, f.group.Close())
	fixturekit.Must(t, f.b.Close())
	bProject := f.cfg.Project
	bProject.ID = charterProjectB
	f.b, err = trace.Open(f.cfg.Root, bProject)
	fixturekit.Must(t, err)
	defer f.b.Close()
	out, api := controller.RecordCharterDecision(ctx, f.a.Project(), fixturekit.Stream, f.a, q, CharterDecisionRequest{Decision: trace.CharterRatify}, OwnerActor, "owner-charter")
	if api != nil || out.State != trace.CharterRatified {
		t.Fatalf("ratify: %+v %+v", out, api)
	}
	writer := Charterer{Repository: f.b, S: &ChartererDependencies{Now: func() time.Time { return fixturekit.DemoStart }, Step: func(string) error { return nil }}}
	fixturekit.Must(t, writer.Pass(ctx))
	fixturekit.Must(t, writer.Pass(ctx))
	afterA, err := f.a.Charter(ctx)
	fixturekit.Must(t, err)
	afterB, err := f.b.Charter(ctx)
	fixturekit.Must(t, err)
	if afterA != beforeA || afterB.Revision != beforeB.Revision+1 || !strings.Contains(afterB.Content, proposal.Proposal.Rule) {
		t.Fatalf("wrong charter revised: A=%+v B=%+v", afterA, afterB)
	}
}

func TestCrossProjectCharterConflictCannotBeDismissed(t *testing.T) {
	f := newCrossCharterFixture(t)
	ctx := context.Background()
	member := "committee"
	id := shed.ObjectionID(1, member, 1)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "shed-ready", Revision: 1, Project: f.a.Project(), Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: OwnerActor, Cause: "test"}
	_, err := f.a.SetFeatureState(ctx, h, InShedState, "Ready for owner ratification.")
	fixturekit.Must(t, err)
	h.ID = "shed-concluded"
	_, err = f.a.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: h, Subject: ShedSubject, To: "concluded-1", Reason: "Debate concluded."}})
	fixturekit.Must(t, err)
	record := shed.Record{Version: shed.Version, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, Member: member, Objections: []shed.Objection{{ID: id, Kind: shed.CharterConflict, Part: "spec#1", Argument: "Rules conflict.", Citations: []string{"charter#1", string(charterProjectB) + "/charter#1"}}}}
	data, err := shed.Encode(record)
	fixturekit.Must(t, err)
	h.Schema, h.ID = "osmia.trace.document", shed.DocumentID(1, member)
	fixturekit.Must(t, f.a.RecordDocuments(ctx, []trace.Document{{Header: h, Path: shed.Path(1, member), Content: string(data)}}))
	host := f.workgroup.Members[0]
	_, err = f.group.EnsureChiefOfStaff(ctx, host, fixturekit.DemoStart, OwnerActor)
	fixturekit.Must(t, err)
	scope := claimCharterChief(t, f.group, host)
	controls := Controls{Load: func() *Controller { return New(Dependencies{}) }}
	tool := controls.ResolveObjection(f.group, scope)
	out, err := tool.Handle(ctx, json.RawMessage(`{"workstream":"`+string(fixturekit.Stream)+`","objection":"`+id+`","decision":"dismiss","note":"Ignore it."}`))
	fixturekit.Must(t, err)
	if !strings.Contains(string(out), `"recorded":false`) || !strings.Contains(string(out), "chief cannot resolve") {
		t.Fatalf("chief dismissal tool: %s", out)
	}

	conflict := shed.Objection{ID: "conflict", Kind: shed.CharterConflict, Part: "spec#1", Argument: "Rules conflict.", Citations: []string{"charter#1", string(charterProjectB) + "/charter#1"}}
	records := []shed.Record{{Version: shed.Version, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, Member: "committee", Objections: []shed.Objection{conflict}}}
	decisions := []shed.ChiefDecisions{{Decisions: []shed.ChiefDecision{{Objection: "conflict", Decision: shed.Dismiss, Note: "Ignore it."}}}}
	entries := shed.DissentRecord(records, nil, decisions)
	if len(entries) != 1 || !entries[0].Blocking || !entries[0].Escalated || entries[0].Disposition != "" || len(shed.ForOwner(entries)) != 1 || len(shed.Undecided(entries)) != 0 {
		t.Fatalf("chief dismissed conflict: %+v", entries)
	}
}

func claimCharterChief(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_events", Revision: 1, Project: repo.Project(), Workstream: stream, At: fixturekit.DemoStart, Actor: OwnerActor, Cause: "test"}
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "events", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Resolve conflicts."})
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token-events", filepath.Join(t.TempDir(), "events"), fixturekit.DemoStart)
	fixturekit.Must(t, err)
	return coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Thread: trace.ChiefOfStaff, Turn: "events", Role: trace.ChiefOfStaff}
}
