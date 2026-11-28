package intakejourney

import (
	context "context"
	fmt "fmt"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	jevtest "github.com/kpenfound/agentcore/jev/jevtest"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	intake "github.com/kpenfound/osmia/internal/intake"
	jev "github.com/kpenfound/osmia/internal/jev"
	jevfixture "github.com/kpenfound/osmia/internal/jev/jevfixture"
	service "github.com/kpenfound/osmia/internal/service"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestManagerDecidesObjectionsBeforeTheOwnerSeesThem(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 1)
	defer f.Stop(t)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	veto := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	advice := shed.ObjectionID(1, intake.CommitteeAgent(2), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Charter, "spec#2", "charter#1"))
	f.Member(1, 2, 1, intakefixture.Objects(p, shed.Fit, "plan", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(p, "Each upload is one small change with its own test.", veto, advice))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	blocked := fmt.Sprintf("do not ratify yet: ratification is blocked by 1 objection (%s); overrule or sustain each one, or ask for a redraft", veto)
	f.AwaitPacket(t, stream, blocked)

	if notice := f.Concluded(t, stream, 1).Event.Body; !strings.Contains(notice, "decide 2 objections with resolve_objection: "+veto+", "+advice+".") {
		t.Fatalf("notice %q asks the manager to decide nothing", notice)
	}
	if entries := f.InboxEntries(t, stream, api.InboxRatification); len(entries) != 0 {
		t.Fatalf("the packet reached the owner before the manager decided its objections: %+v", entries)
	}

	tool := f.ManagerObjectionTool(t, stream)
	if out := intakefixture.Handle(t, tool, `{"objection":"`+veto+`","decision":"dismiss","note":"Answered."}`); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
		t.Fatalf("dismissing before decide: %s", out)
	}
	f.ObjectionDecided(t, stream, advice)
	if out := intakefixture.Handle(t, tool, `{"objection":"`+veto+`","decision":"dismiss","note":"Answered."}`); !strings.Contains(out, "call decide") {
		t.Fatalf("dismissing on another objection's result: %s", out)
	}
	f.ObjectionDecided(t, stream, veto)
	for input, want := range map[string]string{
		`{"objection":"` + veto + `","decision":"dismiss","note":" "}`:     "a note is required",
		`{"objection":"nobody-r1-1","decision":"escalate","note":"Look."}`: "no objection nobody-r1-1 stands",
	} {
		if out := intakefixture.Handle(t, tool, input); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, want) {
			t.Fatalf("%s: %s, want a refusal saying %q", input, out, want)
		}
	}
	dismissal := "The charter asks for small tested changes, and the architect's reply shows each unit is one with its own test."
	if out := intakefixture.Handle(t, tool, `{"objection":"`+veto+`","decision":"dismiss","note":"`+dismissal+`"}`); !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"blocking":false`) {
		t.Fatalf("dismissing the veto: %s", out)
	}
	if out := intakefixture.Handle(t, tool, `{"objection":"`+veto+`","decision":"escalate","note":"Twice."}`); !strings.Contains(out, "already decided") {
		t.Fatalf("a second decision on one objection was recorded: %s", out)
	}

	if entries := f.InboxEntries(t, stream, api.InboxRatification); len(entries) != 0 {
		t.Fatalf("the packet reached the owner with an objection undecided: %+v", entries)
	}
	escalation := "The plan builds uploads before the index the design starts from; I recommend ratifying, since the order does not change what is delivered."
	if out := intakefixture.Handle(t, tool, `{"objection":"`+advice+`","decision":"escalate","note":"`+escalation+`"}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("escalating the advice: %s", out)
	}

	ratifiable := "ratify: nothing blocks, and 2 objections stand as advice on the record"
	packet, _ := f.AwaitDecided(t, stream)
	if packet.Recommendation != ratifiable {
		t.Fatalf("the packet recommends %q, want %q", packet.Recommendation, ratifiable)
	}
	byID := map[string]shed.Entry{}
	for _, e := range packet.Dissent {
		byID[e.ID] = e
	}
	if e := byID[veto]; e.Blocking || e.Disposition != shed.Dismissed || e.By != trace.ManagerDisplay || e.Note != dismissal || e.Escalated {
		t.Fatalf("the dismissed veto in the packet %+v", e)
	}
	if e := byID[advice]; e.Blocking || e.Disposition != "" || e.By != "" || !e.Escalated || e.Escalation != escalation {
		t.Fatalf("the escalated advice in the packet %+v", e)
	}
	var shown []shed.Entry
	for _, e := range packet.Dissent {
		if e.By != trace.ManagerDisplay {
			shown = append(shown, e)
		}
	}
	if len(shown) != 1 || shown[0].ID != advice {
		t.Fatalf("the owner is shown %+v", shown)
	}
	if entries := f.InboxEntries(t, stream, api.InboxRatification); len(entries) != 1 || !slices.Equal(entries[0].Options, []string{"ratify"}) || entries[0].Recommendation != ratifiable {
		t.Fatalf("inbox entries once every objection is decided %+v", entries)
	}
	docs := f.Documents(t, stream, shed.ManagerDocumentID(1))
	if len(docs) != 2 || docs[1].Path != shed.ManagerPath(1) || docs[1].Actor != intake.ManagerActor {
		t.Fatalf("the manager's decision documents %+v", docs)
	}
	actions, err := service.ManagerActions(f.Repository(), stream)
	fixturekit.Must(t, err)
	var texts []string
	for _, a := range actions {
		texts = append(texts, a.Text)
	}
	if !slices.Contains(texts, "Dismissed objection "+veto+" on your behalf: "+dismissal) || !slices.Contains(texts, "Raised objection "+advice+" to you: "+escalation) {
		t.Fatalf("conversation actions %q", texts)
	}

	if _, err := f.C.ShedRule(ctx, stream, veto, "sustain", "The charter rule applies here."); err != nil {
		t.Fatal(err)
	}
	f.AwaitPacket(t, stream, blocked)
	for _, e := range f.Dissent(t, stream) {
		if e.ID == veto && (!e.Blocking || e.Disposition != shed.Sustained || e.By != "" || e.Note != "The charter rule applies here.") {
			t.Fatalf("the veto after the owner sustained it %+v", e)
		}
	}

	objected, err := f.C.ShedObject(ctx, stream, "Name the upload limits.")
	fixturekit.Must(t, err)
	if _, _, apiErr := f.S.Intake().ObjectionSubject(f.Repository(), stream, objected.Objection); apiErr == nil || !strings.Contains(apiErr.Message, "the owner's own") {
		t.Fatalf("the owner's objection is eligible: %+v", apiErr)
	}
	if res, err := jev.Recorded(f.Repository(), stream, jev.KindObjection, jev.ObjectionKey(objected.Objection)); err != nil || res.Status != jev.None {
		t.Fatalf("a judgment was recorded for the owner's objection: %+v %v", res, err)
	}
	if out := intakefixture.Handle(t, tool, `{"objection":"`+objected.Objection+`","decision":"dismiss","note":"Answered."}`); !strings.Contains(out, "the owner's own") {
		t.Fatalf("the manager decided the owner's objection: %s", out)
	}
	if entries := f.InboxEntries(t, stream, api.InboxRatification); len(entries) != 1 || len(entries[0].Options) != 0 {
		t.Fatalf("inbox entries with the owner's objection %+v", entries)
	}
}

func TestAConfidentObjectionPickBindsTheManagersDecision(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 1)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	veto := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	advice := shed.ObjectionID(1, intake.CommitteeAgent(2), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Charter, "spec#2", "charter#1"))
	f.Member(1, 2, 1, intakefixture.Objects(p, shed.Fit, "plan", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(p, "Each upload is one small change with its own test.", veto, advice))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)

	provider := &jevtest.Provider{Results: []jevtest.Result{jevfixture.Pick("dismiss", 0.96, "escalate")}}
	judge := jevfixture.Answering(f.Clock.Now, provider)
	res := jevfixture.Decide(judge, f.Repository(), f.Project, stream, jev.KindObjection, jev.ObjectionKey(veto), "dismiss", "escalate")
	if res.Status != jev.Binding || res.Pick != "dismiss" {
		t.Fatalf("result %+v", res)
	}
	tool := f.ManagerObjectionTool(t, stream)
	if out := intakefixture.Handle(t, tool, `{"objection":"`+veto+`","decision":"escalate","note":"Owner."}`); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "Jev decided dismiss") || !strings.Contains(out, res.Judgment) {
		t.Fatalf("escalating against the pick: %s", out)
	}
	for _, e := range f.Dissent(t, stream) {
		if e.ID == veto && (e.Escalated || e.Disposition != "") {
			t.Fatalf("a refused decision changed the objection: %+v", e)
		}
	}
	again := tool
	if out := intakefixture.Handle(t, again, `{"objection":"`+veto+`","decision":"escalate","note":"Owner."}`); !strings.Contains(out, "Jev decided dismiss") {
		t.Fatalf("a later turn: %s", out)
	}
	dismissal := "The architect's reply shows each unit is one small change with its own test."
	if out := intakefixture.Handle(t, again, `{"objection":"`+veto+`","decision":"dismiss","note":"`+dismissal+`"}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("dismissing as picked: %s", out)
	}
	rounds, err := shed.AllManagerDecisions(f.Repository(), stream)
	fixturekit.Must(t, err)
	var recorded shed.ManagerDecision
	for _, r := range rounds {
		for _, d := range r.Decisions {
			if d.Objection == veto {
				recorded = d
			}
		}
	}
	if recorded.Decision != shed.Dismiss || recorded.Note != dismissal || recorded.DecidedBy != trace.DecidedByJev || recorded.Judgment != res.Judgment {
		t.Fatalf("the recorded decision %+v", recorded)
	}
	if len(provider.Requests()) != 1 {
		t.Fatalf("%d requests", len(provider.Requests()))
	}
}

func decideObjection(t *testing.T, tool coreadapter.Tool, objection string) string {
	t.Helper()
	return intakefixture.Handle(t, tool, `{"kind":"objection","subject":"`+objection+`","evidence":"The architect's reply answers it."}`)
}

func TestDecideRefusesObjectionsThatAreTheOwners(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 1)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	veto := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Charter, "spec#2", "charter#1"))
	f.Member(1, 2, 1, intakefixture.Objects(p, shed.Fit, "plan", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(p, "Each upload is one small change with its own test.", veto))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)

	repo := f.Repository()
	records, err := shed.Records(repo, stream)
	fixturekit.Must(t, err)
	conflict := shed.ObjectionID(1, intake.CommitteeAgent(3), 1)
	data, err := shed.Encode(shed.Record{Version: shed.Version, Round: 1, Member: intake.CommitteeAgent(3), Revision: records[0].Revision, Concessions: []shed.Concession{},
		Objections: []shed.Objection{{ID: conflict, Kind: shed.CharterConflict, Part: "spec#1", Argument: "Two projects' charters conflict.", Citations: []string{"charter#1", "p_other/charter#1"}}}})
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.DocumentID(1, intake.CommitteeAgent(3)), Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: trace.Actor{Kind: "agent", ID: intake.CommitteeAgent(3)}, Cause: "test"}, Path: shed.Path(1, intake.CommitteeAgent(3)), Content: string(data)}}))
	objected, err := f.C.ShedObject(context.Background(), stream, "Name the upload limits.")
	fixturekit.Must(t, err)

	provider := &jevtest.Provider{Results: []jevtest.Result{jevfixture.Pick("dismiss", 0.96, "escalate")}}
	service.InspectForTest(f.S.Service).SetJudge(jevfixture.Answering(f.Clock.Now, provider))
	decide := f.ManagerDecideTool(t, stream, "events_2")
	for objection, want := range map[string]string{conflict: "conflict between member charters", objected.Objection: "the owner's own"} {
		if out := decideObjection(t, decide, objection); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, want) {
			t.Fatalf("decide %s: %s, want a refusal saying %q", objection, out, want)
		}
		if res, err := jev.Recorded(repo, stream, jev.KindObjection, jev.ObjectionKey(objection)); err != nil || res.Status != jev.None {
			t.Fatalf("a judgment is recorded for %s: %+v %v", objection, res, err)
		}
	}
	if n := decideRequests(provider); n != 0 {
		t.Fatalf("%d decide requests for objections that are the owner's", n)
	}
	if out := decideObjection(t, decide, veto); !strings.Contains(out, `"decision":"binding"`) || !strings.Contains(out, `"pick":"dismiss"`) {
		t.Fatalf("decide the committee's objection: %s", out)
	}
	if n := decideRequests(provider); n != 1 {
		t.Fatalf("%d decide requests", n)
	}
}

func decideRequests(p *jevtest.Provider) int {
	n := 0
	for _, r := range p.Requests() {
		for _, o := range r.Questions["decision"].Options {
			if o.Name == "escalate" {
				n++
				break
			}
		}
	}
	return n
}

func TestAnObjectionDecisionIsEnteredAsTheManagerMadeIt(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 1)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	veto := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	advice := shed.ObjectionID(1, intake.CommitteeAgent(2), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Charter, "spec#2", "charter#1"))
	f.Member(1, 2, 1, intakefixture.Objects(p, shed.Fit, "plan", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(p, "Each upload is one small change with its own test.", veto, advice))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	var judged atomic.Int32
	jevp := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		if a.Trigger == intake.TriggerObjectionDecision {
			judged.Add(1)
		}
		return jevfixture.DefaultFits(a)
	}}
	service.InspectForTest(f.S.Service).SetRouterJudge(jevfixture.TransitionJudge(f.Clock.Now, jevp))
	f.ObjectionDecided(t, stream, veto)
	f.ObjectionDecided(t, stream, advice)
	tool := f.ManagerObjectionTool(t, stream)

	if out := intakefixture.Handle(t, tool, `{"objection":"`+veto+`","decision":"dismiss","note":"The architect's reply answers it."}`); !strings.Contains(out, `"recorded":true`) || strings.Contains(out, "departed") {
		t.Fatalf("dismissing: %s", out)
	}
	moved := f.ObjectionTransition(t, stream, veto)
	if moved.To != "dismissed" || moved.DecidedBy != trace.DecidedByManager || moved.Departed() || moved.Judgment != "" {
		t.Fatalf("the dismissal %+v", moved)
	}
	if out := intakefixture.Handle(t, tool, `{"objection":"`+advice+`","decision":"escalate","note":"The plan builds uploads before the index."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("escalating: %s", out)
	}
	if moved := f.ObjectionTransition(t, stream, advice); moved.To != "escalated" || moved.DecidedBy != trace.DecidedByManager {
		t.Fatalf("the escalation %+v", moved)
	}
	if n := judged.Load(); n != 0 {
		t.Fatalf("Jev was asked %d times about the chief's decisions", n)
	}
}
