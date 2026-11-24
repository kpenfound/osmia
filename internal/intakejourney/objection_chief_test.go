package intakejourney

import (
	context "context"
	fmt "fmt"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	jevtest "github.com/kpenfound/agentcore/jev/jevtest"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	intake "github.com/kpenfound/osmia/internal/intake"
	jev "github.com/kpenfound/osmia/internal/jev"
	jevfixture "github.com/kpenfound/osmia/internal/jev/jevfixture"
	service "github.com/kpenfound/osmia/internal/service"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

// chiefDecides records the chief of staff's decision on one committee
// objection, as its resolve_objection tool records it.
func (f *shedFixture) chiefDecides(t *testing.T, stream config.WorkstreamID, objection string, decision shed.Decide, note string) shed.Entry {
	t.Helper()
	entry, api := f.s.decideObjection(context.Background(), f.repository(), stream, objectionDecisionRequest{Objection: objection, Decision: decision, Note: note, Turn: "events_1"})
	if api != nil {
		t.Fatal(api.Message)
	}
	return entry
}

// awaitDecided waits until the packet the API serves carries a decision of
// the chief of staff or a disposition on every committee objection of its
// dissent record, and returns it with its revision.
func (f *shedFixture) awaitDecided(t *testing.T, stream config.WorkstreamID) (shed.Packet, int) {
	t.Helper()
	wait, close := serviceChanges(t, f.s)
	defer close()
	deadline := time.Now().Add(demoTimeout)
	for {
		out, err := f.c.Packet(context.Background(), stream)
		if err != nil && !failed(err, NotFound) {
			t.Fatal(err)
		}
		if err == nil && len(shed.Undecided(out.Packet.Dissent)) == 0 {
			return out.Packet, out.Revision
		}
		if time.Now().After(deadline) {
			t.Fatalf("the packet of workstream %s is %+v %v, want every committee objection decided", stream, out.Packet, err)
		}
		wait()
	}
}

// chiefObjectionTool claims a chief-of-staff turn of the workstream and
// returns its resolve_objection tool.
func (f *shedFixture) chiefObjectionTool(t *testing.T, stream config.WorkstreamID) coreadapter.Tool {
	t.Helper()
	repo := f.repository()
	scope := intakefixture.ClaimChiefTurn(t, repo, f.project, stream, f.clock.Now, intakefixture.ChiefTurnInput{
		Turn: "events_1", Prompt: "Debate concluded.", Actor: serviceActor, Cause: "fixture", Token: "token-events", Directory: "events",
	})
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	return controls.resolveObjection(repo, scope)
}

var decideWith = intakefixture.DecideWith

// objectionDecided leaves the decide result of the objection as a fallback, as
// the decide tool does while Jev is unconfigured.
func (f *shedFixture) objectionDecided(t *testing.T, stream config.WorkstreamID, objection string) {
	t.Helper()
	jevfixture.Fallback(f.repository(), f.project, stream, jev.KindObjection, jev.ObjectionKey(objection))
}

// The committee's objections go to the chief of staff before the owner: the
// ratification reaches the owner's inbox only once each one is decided, a
// charter veto the chief of staff dismisses no longer blocks and is not put
// to the owner, what it escalates is, and the owner's own ruling and
// objections stay the owner's.
func TestChiefOfStaffDecidesObjectionsBeforeTheOwnerSeesThem(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 1)
	defer f.stop(t)
	ctx := context.Background()
	p := &faults{}
	veto := shed.ObjectionID(1, committeeAgent(1), 1)
	advice := shed.ObjectionID(1, committeeAgent(2), 1)
	f.member(1, 1, 1, objects(p, shed.Charter, "spec#2", "charter#1"))
	f.member(1, 2, 1, objects(p, shed.Fit, "plan", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "Each upload is one small change with its own test.", veto, advice))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.Check(t)
	blocked := fmt.Sprintf("do not ratify yet: ratification is blocked by 1 objection (%s); overrule or sustain each one, or ask for a redraft", veto)
	f.awaitPacket(t, stream, blocked)

	if notice := f.concluded(t, stream, 1).Event.Body; !strings.Contains(notice, "decide 2 objections with resolve_objection: "+veto+", "+advice+".") {
		t.Fatalf("notice %q asks the chief of staff to decide nothing", notice)
	}
	if entries := f.inboxEntries(t, stream, InboxRatification); len(entries) != 0 {
		t.Fatalf("the packet reached the owner before the chief of staff decided its objections: %+v", entries)
	}

	tool := f.chiefObjectionTool(t, stream)
	// The tool records nothing before decide, nor on another objection's result.
	if out := decideWith(t, tool, `{"objection":"`+veto+`","decision":"dismiss","note":"Answered."}`); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "call decide") {
		t.Fatalf("dismissing before decide: %s", out)
	}
	f.objectionDecided(t, stream, advice)
	if out := decideWith(t, tool, `{"objection":"`+veto+`","decision":"dismiss","note":"Answered."}`); !strings.Contains(out, "call decide") {
		t.Fatalf("dismissing on another objection's result: %s", out)
	}
	f.objectionDecided(t, stream, veto)
	for input, want := range map[string]string{
		`{"objection":"` + veto + `","decision":"dismiss","note":" "}`:     "a note is required",
		`{"objection":"nobody-r1-1","decision":"escalate","note":"Look."}`: "no objection nobody-r1-1 stands",
	} {
		if out := decideWith(t, tool, input); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, want) {
			t.Fatalf("%s: %s, want a refusal saying %q", input, out, want)
		}
	}
	dismissal := "The charter asks for small tested changes, and the architect's reply shows each unit is one with its own test."
	if out := decideWith(t, tool, `{"objection":"`+veto+`","decision":"dismiss","note":"`+dismissal+`"}`); !strings.Contains(out, `"recorded":true`) || !strings.Contains(out, `"blocking":false`) {
		t.Fatalf("dismissing the veto: %s", out)
	}
	if out := decideWith(t, tool, `{"objection":"`+veto+`","decision":"escalate","note":"Twice."}`); !strings.Contains(out, "already decided") {
		t.Fatalf("a second decision on one objection was recorded: %s", out)
	}

	if entries := f.inboxEntries(t, stream, InboxRatification); len(entries) != 0 {
		t.Fatalf("the packet reached the owner with an objection undecided: %+v", entries)
	}
	escalation := "The plan builds uploads before the index the design starts from; I recommend ratifying, since the order does not change what is delivered."
	if out := decideWith(t, tool, `{"objection":"`+advice+`","decision":"escalate","note":"`+escalation+`"}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("escalating the advice: %s", out)
	}

	ratifiable := "ratify: nothing blocks, and 2 objections stand as advice on the record"
	packet, _ := f.awaitDecided(t, stream)
	if packet.Recommendation != ratifiable {
		t.Fatalf("the packet recommends %q, want %q", packet.Recommendation, ratifiable)
	}
	byID := map[string]shed.Entry{}
	for _, e := range packet.Dissent {
		byID[e.ID] = e
	}
	if e := byID[veto]; e.Blocking || e.Disposition != shed.Dismissed || e.By != shed.ChiefOfStaff || e.Note != dismissal || e.Escalated {
		t.Fatalf("the dismissed veto in the packet %+v", e)
	}
	if e := byID[advice]; e.Blocking || e.Disposition != "" || e.By != "" || !e.Escalated || e.Escalation != escalation {
		t.Fatalf("the escalated advice in the packet %+v", e)
	}
	if shown := shed.ForOwner(packet.Dissent); len(shown) != 1 || shown[0].ID != advice {
		t.Fatalf("the owner is shown %+v", shown)
	}
	if entries := f.inboxEntries(t, stream, InboxRatification); len(entries) != 1 || !slices.Equal(entries[0].Options, []string{"ratify"}) || entries[0].Recommendation != ratifiable {
		t.Fatalf("inbox entries once every objection is decided %+v", entries)
	}
	docs := f.documents(t, stream, shed.ChiefDocumentID(1))
	if len(docs) != 2 || docs[1].Path != shed.ChiefPath(1) || docs[1].Actor != chiefActor {
		t.Fatalf("the chief of staff's decision documents %+v", docs)
	}
	actions, err := chiefActions(f.repository(), stream)
	must(t, err)
	var texts []string
	for _, a := range actions {
		texts = append(texts, a.Text)
	}
	if !slices.Contains(texts, "Dismissed objection "+veto+" on your behalf: "+dismissal) || !slices.Contains(texts, "Raised objection "+advice+" to you: "+escalation) {
		t.Fatalf("conversation actions %q", texts)
	}

	if _, err := f.c.ShedRule(ctx, stream, veto, "sustain", "The charter rule applies here."); err != nil {
		t.Fatal(err)
	}
	f.awaitPacket(t, stream, blocked)
	for _, e := range f.dissent(t, stream) {
		if e.ID == veto && (!e.Blocking || e.Disposition != shed.Sustained || e.By != "" || e.Note != "The charter rule applies here.") {
			t.Fatalf("the veto after the owner sustained it %+v", e)
		}
	}

	objected, err := f.c.ShedObject(ctx, stream, "Name the upload limits.")
	must(t, err)
	// The owner's own objection is refused before Jev is asked: no judgment is
	// recorded for it.
	if _, _, api := f.s.intake().ObjectionSubject(f.repository(), stream, objected.Objection); api == nil || !strings.Contains(api.Message, "the owner's own") {
		t.Fatalf("the owner's objection is eligible: %+v", api)
	}
	if res, err := jev.Recorded(f.repository(), stream, jev.KindObjection, jev.ObjectionKey(objected.Objection)); err != nil || res.Status != jev.None {
		t.Fatalf("a judgment was recorded for the owner's objection: %+v %v", res, err)
	}
	if out := decideWith(t, tool, `{"objection":"`+objected.Objection+`","decision":"dismiss","note":"Answered."}`); !strings.Contains(out, "the owner's own") {
		t.Fatalf("the chief of staff decided the owner's objection: %s", out)
	}
	if entries := f.inboxEntries(t, stream, InboxRatification); len(entries) != 1 || len(entries[0].Options) != 0 {
		t.Fatalf("inbox entries with the owner's objection %+v", entries)
	}
}

// A confident pick for an objection binds resolve_objection to the picked
// outcome with the chief's own note, and the decision is recorded as Jev's with
// its judgment. The binding holds in a later turn and after a restart with no
// new request.
func TestAConfidentObjectionPickBindsTheChiefsDecision(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 1)
	defer f.stop(t)
	p := &faults{}
	veto := shed.ObjectionID(1, committeeAgent(1), 1)
	advice := shed.ObjectionID(1, committeeAgent(2), 1)
	f.member(1, 1, 1, objects(p, shed.Charter, "spec#2", "charter#1"))
	f.member(1, 2, 1, objects(p, shed.Fit, "plan", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "Each upload is one small change with its own test.", veto, advice))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.Check(t)

	provider := &jevtest.Provider{Results: []jevtest.Result{jevfixture.Pick("dismiss", 0.96, "escalate")}}
	judge := jevfixture.Answering(f.clock.Now, provider)
	res := jevfixture.Decide(judge, f.repository(), f.project, stream, jev.KindObjection, jev.ObjectionKey(veto), "dismiss", "escalate")
	if res.Status != jev.Binding || res.Pick != "dismiss" {
		t.Fatalf("result %+v", res)
	}
	tool := f.chiefObjectionTool(t, stream)
	if out := decideWith(t, tool, `{"objection":"`+veto+`","decision":"escalate","note":"Owner."}`); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "Jev decided dismiss") || !strings.Contains(out, res.Judgment) {
		t.Fatalf("escalating against the pick: %s", out)
	}
	for _, e := range f.dissent(t, stream) {
		if e.ID == veto && (e.Escalated || e.Disposition != "") {
			t.Fatalf("a refused decision changed the objection: %+v", e)
		}
	}
	// The binding is read from the records on every call, so a later call sees
	// the same pick.
	again := tool
	if out := decideWith(t, again, `{"objection":"`+veto+`","decision":"escalate","note":"Owner."}`); !strings.Contains(out, "Jev decided dismiss") {
		t.Fatalf("a later turn: %s", out)
	}
	dismissal := "The architect's reply shows each unit is one small change with its own test."
	if out := decideWith(t, again, `{"objection":"`+veto+`","decision":"dismiss","note":"`+dismissal+`"}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("dismissing as picked: %s", out)
	}
	rounds, err := shed.AllChiefDecisions(f.repository(), stream)
	must(t, err)
	var recorded shed.ChiefDecision
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

// chiefDecideTool claims a chief-of-staff turn of the workstream and returns
// the service's decide tool.
func (f *shedFixture) chiefDecideTool(t *testing.T, stream config.WorkstreamID, turn string) coreadapter.Tool {
	t.Helper()
	repo := f.repository()
	scope := intakefixture.ClaimChiefTurn(t, repo, f.project, stream, f.clock.Now, intakefixture.ChiefTurnInput{
		Turn: turn, Prompt: "Debate concluded.", Actor: serviceActor, Cause: "fixture", Token: "token-" + turn, Directory: turn,
	})
	return service.InspectForTest(f.s.Service).DecideTool(repo, scope)
}

// decide asks the decide tool about the objection.
func decideObjection(t *testing.T, tool coreadapter.Tool, objection string) string {
	t.Helper()
	return decideWith(t, tool, `{"kind":"objection","subject":"`+objection+`","evidence":"The architect's reply answers it."}`)
}

// A conflict between member charters and the owner's own objection are the
// owner's: decide refuses them as ordinary results with no decide request and
// no judgment of the chief's kind, and an objection the chief of staff may
// decide is judged. Jev decides the owner's at the owner's gate instead.
func TestDecideRefusesObjectionsThatAreTheOwners(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 1)
	defer f.stop(t)
	p := &faults{}
	veto := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Charter, "spec#2", "charter#1"))
	f.member(1, 2, 1, objects(p, shed.Fit, "plan", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "Each upload is one small change with its own test.", veto))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.Check(t)

	repo := f.repository()
	records, err := shed.Records(repo, stream)
	must(t, err)
	conflict := shed.ObjectionID(1, committeeAgent(3), 1)
	data, err := shed.Encode(shed.Record{Version: shed.Version, Round: 1, Member: committeeAgent(3), Revision: records[0].Revision, Concessions: []shed.Concession{},
		Objections: []shed.Objection{{ID: conflict, Kind: shed.CharterConflict, Part: "spec#1", Argument: "Two projects' charters conflict.", Citations: []string{"charter#1", "p_other/charter#1"}}}})
	must(t, err)
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.DocumentID(1, committeeAgent(3)), Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: trace.Actor{Kind: "agent", ID: committeeAgent(3)}, Cause: "test"}, Path: shed.Path(1, committeeAgent(3)), Content: string(data)}}))
	objected, err := f.c.ShedObject(context.Background(), stream, "Name the upload limits.")
	must(t, err)

	provider := &jevtest.Provider{Results: []jevtest.Result{jevfixture.Pick("dismiss", 0.96, "escalate")}}
	service.InspectForTest(f.s.Service).SetJudge(jevfixture.Answering(f.clock.Now, provider))
	decide := f.chiefDecideTool(t, stream, "events_2")
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

// decideRequests counts the requests of the chief of staff's decide on an
// objection, whose options are dismiss and escalate. The owner's gate on the
// owner's own objection and on a charter conflict asks with its own options.
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

// objectionTransition returns the latest transition of the objection's subject.
func (f *shedFixture) objectionTransition(t *testing.T, stream config.WorkstreamID, objection string) trace.Transition {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](f.repository(), stream)
	must(t, err)
	var out trace.Transition
	for _, tr := range transitions {
		if tr.Subject == "objection-"+objection {
			out = tr
		}
	}
	return out
}

// The chief of staff's decision on an objection is its own: a pinned move that
// enters the decision as the chief made it, asks Jev nothing and records the
// chief of staff as the decider.
func TestAnObjectionDecisionIsEnteredAsTheChiefMadeIt(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 1)
	defer f.stop(t)
	p := &faults{}
	veto := shed.ObjectionID(1, committeeAgent(1), 1)
	advice := shed.ObjectionID(1, committeeAgent(2), 1)
	f.member(1, 1, 1, objects(p, shed.Charter, "spec#2", "charter#1"))
	f.member(1, 2, 1, objects(p, shed.Fit, "plan", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "Each upload is one small change with its own test.", veto, advice))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.Check(t)
	var judged atomic.Int32
	jevp := &jevfixture.Transitions{Script: func(a jevfixture.Ask) jevfixture.Reply {
		if a.Trigger == intake.TriggerObjectionDecision {
			judged.Add(1)
		}
		return jevfixture.DefaultFits(a)
	}}
	service.InspectForTest(f.s.Service).SetRouterJudge(jevfixture.TransitionJudge(f.clock.Now, jevp))
	f.objectionDecided(t, stream, veto)
	f.objectionDecided(t, stream, advice)
	tool := f.chiefObjectionTool(t, stream)

	if out := decideWith(t, tool, `{"objection":"`+veto+`","decision":"dismiss","note":"The architect's reply answers it."}`); !strings.Contains(out, `"recorded":true`) || strings.Contains(out, "departed") {
		t.Fatalf("dismissing: %s", out)
	}
	moved := f.objectionTransition(t, stream, veto)
	if moved.To != "dismissed" || moved.DecidedBy != trace.DecidedByChief || moved.Departed() || moved.Judgment != "" {
		t.Fatalf("the dismissal %+v", moved)
	}
	if out := decideWith(t, tool, `{"objection":"`+advice+`","decision":"escalate","note":"The plan builds uploads before the index."}`); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("escalating: %s", out)
	}
	if moved := f.objectionTransition(t, stream, advice); moved.To != "escalated" || moved.DecidedBy != trace.DecidedByChief {
		t.Fatalf("the escalation %+v", moved)
	}
	if n := judged.Load(); n != 0 {
		t.Fatalf("Jev was asked %d times about the chief's decisions", n)
	}
}
