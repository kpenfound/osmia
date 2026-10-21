package intakejourney

import (
	context "context"

	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	fmt "fmt"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	shed "github.com/kpenfound/osmia/internal/shed"
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
	if out := decideWith(t, tool, `{"objection":"`+objected.Objection+`","decision":"dismiss","note":"Answered."}`); !strings.Contains(out, "the owner's own") {
		t.Fatalf("the chief of staff decided the owner's objection: %s", out)
	}
	if entries := f.inboxEntries(t, stream, InboxRatification); len(entries) != 1 || len(entries[0].Options) != 0 {
		t.Fatalf("inbox entries with the owner's objection %+v", entries)
	}
}
