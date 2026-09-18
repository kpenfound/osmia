package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// awaitPacket waits until the packet the API serves recommends what the
// dissent record now calls for, and returns it. A workstream at its decision
// point has no packet until the pass that presents it records one.
func (f *shedFixture) awaitPacket(t *testing.T, stream config.WorkstreamID, recommendation string) shed.Packet {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		out, err := f.c.Packet(context.Background(), stream)
		if err != nil && !failed(err, NotFound) {
			t.Fatal(err)
		}
		if err == nil && out.Packet.Recommendation == recommendation {
			if out.Workstream != stream || out.Project != f.project || out.Revision < 1 {
				t.Fatalf("packet response %+v", out)
			}
			return out.Packet
		}
		if time.Now().After(deadline) {
			t.Fatalf("the packet of workstream %s is %+v %v, want the recommendation %q", stream, out.Packet, err, recommendation)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (f *shedFixture) ratification(t *testing.T, stream config.WorkstreamID, round int) shed.Ratification {
	t.Helper()
	docs := f.documents(t, stream, shed.RatificationDocumentID(round))
	if len(docs) != 1 || docs[0].Path != shed.RatificationPath(round) || docs[0].Actor != ownerActor || docs[0].Cause != "owner-shed" {
		t.Fatalf("ratification documents of round %d: %+v", round, docs)
	}
	record, err := shed.ParseRatification([]byte(docs[0].Content))
	must(t, err)
	return record
}

// The packet is presented when debate concludes, follows the owner's
// dispositions, and what it presents is what ratification is judged against:
// the charter veto blocks until the owner overrules it.
func TestPacketPresentsTheDecisionAndTheOverruleUnblocksIt(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 2, 1)
	defer f.stop(t)
	ctx := context.Background()
	p := &faults{}
	veto := shed.ObjectionID(1, committeeAgent(1), 1)
	advice := shed.ObjectionID(1, committeeAgent(2), 1)
	f.member(1, 1, 1, objects(p, shed.Charter, "spec#2", "charter#1"))
	f.member(1, 2, 1, objects(p, shed.Fit, "plan", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "Kept as it is.", veto, advice))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)

	blocked := fmt.Sprintf("do not ratify yet: ratification is blocked by 1 objection (%s); overrule or sustain each one, or ask for a redraft", veto)
	packet := f.awaitPacket(t, stream, blocked)
	conclusion := f.transition(t, stream, "shed-concluded-1").Reason
	if packet.Round != 1 || packet.Revision != (shed.Pin{Spec: 1, Plan: 1}) || packet.Skipped || packet.Conclusion != conclusion {
		t.Fatalf("packet %+v", packet)
	}
	// What blocks comes first, and each entry carries whether it blocks.
	if len(packet.Dissent) != 2 || packet.Dissent[0].ID != veto || !packet.Dissent[0].Blocking || packet.Dissent[1].ID != advice || packet.Dissent[1].Blocking {
		t.Fatalf("the packet's dissent record %+v", packet.Dissent)
	}
	docs := f.documents(t, stream, shed.PacketDocumentID(1))
	if len(docs) != 1 || docs[0].Path != shed.PacketPath(1) || docs[0].Actor != shedActor || docs[0].Cause != "ratification-packet" {
		t.Fatalf("packet documents %+v", docs)
	}
	// The chief of staff is told to present it and to raise the attention
	// item in its status.
	if notice := f.concluded(t, stream, 1).Event.Body; !strings.Contains(notice, presentation(blocked)) {
		t.Fatalf("notice %q, want %q", notice, presentation(blocked))
	}
	// A charter veto with no disposition refuses ratification, naming it.
	_, err := f.c.Ratify(ctx, stream, 1, 1)
	if !failed(err, Conflict) || !strings.Contains(err.Error(), fmt.Sprintf("objection %s (charter, by %s in round 1 on spec#2) blocks and has no disposition", veto, committeeAgent(1))) {
		t.Fatalf("ratifying over a charter veto: %v", err)
	}

	out, err := f.c.ShedOverrule(ctx, stream, veto, "I accept the risk.")
	must(t, err)
	if out.Action != string(shed.Overruled) || out.Objection != veto || out.Round != 1 || !strings.Contains(out.Detail, "I accept the risk.") {
		t.Fatalf("overrule %+v", out)
	}
	if moves := f.ownerMoves(t, stream); !slices.Equal(moves, []string{"overruled-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	entries := f.dissent(t, stream)
	if len(entries) != 2 || entries[0].ID != veto || entries[0].Blocking || entries[0].Disposition != shed.Overruled || entries[0].Note != "I accept the risk." {
		t.Fatalf("dissent after the overrule %+v", entries)
	}
	// The packet the owner reads next says the overrule unblocked it.
	ratifiable := "ratify: nothing blocks, and 2 objections stand as advice on the record"
	if packet := f.awaitPacket(t, stream, ratifiable); len(packet.Dissent) != 2 || packet.Dissent[0].Disposition != shed.Overruled {
		t.Fatalf("the packet after the overrule %+v", packet)
	}
	if docs := f.documents(t, stream, shed.PacketDocumentID(1)); len(docs) != 2 || docs[1].Revision != 2 {
		t.Fatalf("packet documents after the overrule %+v", docs)
	}
	// A disposition the owner takes back, and then takes again, leaves the
	// packet saying what an earlier revision said. It is recorded all the
	// same, because the revision the API serves is the decision in force.
	if _, err := f.c.ShedRule(ctx, stream, veto, "sustain", "On reflection, no."); err != nil {
		t.Fatal(err)
	}
	f.awaitPacket(t, stream, blocked)
	if _, err := f.c.ShedOverrule(ctx, stream, veto, "I accept the risk."); err != nil {
		t.Fatal(err)
	}
	f.awaitPacket(t, stream, ratifiable)
	docs = f.documents(t, stream, shed.PacketDocumentID(1))
	if len(docs) != 4 || docs[3].Revision != 4 || docs[3].Content != docs[1].Content {
		t.Fatalf("packet documents after the owner changed their mind twice %+v", docs)
	}

	ratified, err := f.c.Ratify(ctx, stream, 1, 1)
	must(t, err)
	if ratified.Round != 1 || ratified.Spec != 1 || ratified.Plan != 1 || ratified.Sealing != "requested" || ratified.Workstream != stream {
		t.Fatalf("ratification %+v", ratified)
	}
	if want := "the owner ratified spec.md revision 1 and plan.json revision 1 after round 1, over 1 objection the owner disposed of; the sealing is asked for"; ratified.Detail != want {
		t.Fatalf("detail %q, want %q", ratified.Detail, want)
	}
	record := f.ratification(t, stream, 1)
	if record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || record.Round != 1 || len(record.Dispositions) != 1 ||
		record.Dispositions[0] != (shed.Ruling{Objection: veto, Disposition: shed.Overruled, Note: "I accept the risk."}) || len(record.Dissent) != 2 {
		t.Fatalf("the recorded ratification %+v", record)
	}
	if moves := f.ownerMoves(t, stream); !slices.Equal(moves, []string{"overruled-1", "ruled-1", "overruled-1", "ratified-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	// The gate changes no state of its own: sealing moves the workstream
	// on, and this clone has no upstream remote to seal from, so the sealing
	// stays pending and the workstream in the shed.
	f.stillInShed(t, stream)
	// Ratifying the same revisions again records nothing again and reports
	// the sealing already asked for.
	again, err := f.c.Ratify(ctx, stream, 1, 1)
	must(t, err)
	if !slices.Contains([]string{"requested", "pending", "running"}, again.Sealing) || again.Round != 1 || !strings.HasPrefix(again.Detail, fmt.Sprintf("workstream %s is ratified at %s already; ", stream, shed.Pin{Spec: 1, Plan: 1})) {
		t.Fatalf("ratifying twice %+v", again)
	}
	if docs := f.documents(t, stream, shed.RatificationDocumentID(1)); len(docs) != 1 {
		t.Fatalf("ratifying twice recorded %+v", docs)
	}
	if moves := f.ownerMoves(t, stream); !slices.Equal(moves, []string{"overruled-1", "ruled-1", "overruled-1", "ratified-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	p.check(t)
}

// Ratification lists every reason it is refused, not the first one.
func TestRatifyListsEveryReasonItIsRefused(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	p := &faults{}
	size := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "The unit is one change.", size))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	f.awaitPacket(t, stream, shed.Recommend(f.dissent(t, stream)))

	// A criterion the plan does not address leaves the recorded plan invalid,
	// and moves the spec past the revision the packet named.
	must(t, f.repository().RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.SpecDocument, Revision: 2,
		Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: architectActor, Cause: "test"},
		Path: plan.SpecPath, Content: validSpec + "3. Nobody planned this one.\n"}}))

	_, err := f.c.Ratify(ctx, stream, 1, 1)
	if !failed(err, Conflict) {
		t.Fatalf("ratifying a stale revision of an invalid plan over an objection: %v", err)
	}
	for _, want := range []string{
		"spec.md revision 1 and plan.json revision 1 are ratified, and the current revisions are spec.md revision 2 and plan.json revision 1: read the packet again",
		fmt.Sprintf("objection %s (size, by %s in round 1 on plan#resume) blocks and has no disposition", size, committeeAgent(1)),
		"the plan is not valid: spec#3",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal lacks %q:\n%v", want, err)
		}
	}
	// Sustaining the objection keeps it blocking; the reason says so.
	if _, err := f.c.ShedRule(ctx, stream, size, "sustain", "Split it."); err != nil {
		t.Fatal(err)
	}
	_, err = f.c.Ratify(ctx, stream, 2, 1)
	if !failed(err, Conflict) || !strings.Contains(err.Error(), fmt.Sprintf("objection %s (size, by %s in round 1 on plan#resume) blocks and is sustained and is not conceded", size, committeeAgent(1))) {
		t.Fatalf("ratifying over a sustained objection: %v", err)
	}
	// Revisions are what the packet named, never nothing.
	for name, req := range map[string]RatifyRequest{"no spec": {Plan: 1}, "no plan": {Spec: 1}, "a negative revision": {Spec: 1, Plan: -1}} {
		if _, err := f.c.Ratify(ctx, stream, req.Spec, req.Plan); !failed(err, Validation) {
			t.Fatalf("ratifying with %s: %v", name, err)
		}
	}
}

// A redraft the owner asks for goes back to the architect with the note, and
// the debate the redraft restarts reads what the architect wrote.
func TestRedraftGoesBackToTheArchitectAndDebateResumes(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	p := &faults{}
	size := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "The unit is one change.", size))
	// The redraft is the architect's turn of the request: it reads the
	// owner's note and the dissent that stands, and delivers the split plan.
	redraft := roundInput{Round: 1, Redraft: true}
	f.script(redraft.turnID(1), map[string]string{plan.PlanPath: splitPlan}, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		for _, want := range []string{"the owner read the packet and asked you to redraft spec.md revision 1 and plan.json revision 1", "Split the resume unit into what it addresses.",
			fmt.Sprintf("- %s (size, blocking, by %s in round 1 on plan#resume, citing spec#1): It does not hold.", size, committeeAgent(1))} {
			if !strings.Contains(req.Prompt, want) {
				p.report("the redraft prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		return nil
	})
	f.member(2, 1, 1, concedes(p, size))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")

	// A redraft is the owner's to ask for once debate concluded, with a note.
	if _, err := f.c.ShedRedraft(ctx, stream, "  "); !failed(err, Validation) {
		t.Fatalf("a redraft without a note: %v", err)
	}
	out, err := f.c.ShedRedraft(ctx, stream, "Split the resume unit into what it addresses.")
	must(t, err)
	if out.Action != "redraft" || out.Round != 1 || !strings.Contains(out.Detail, "Split the resume unit into what it addresses.") {
		t.Fatalf("redraft %+v", out)
	}
	asked, err := shed.Redrafts(f.repository(), stream)
	must(t, err)
	if len(asked) != 1 || asked[0].Round != 1 || asked[0].Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("the recorded request %+v", asked)
	}

	f.awaitShed(t, stream, "concluded-2", "concluded-3")
	p.check(t)
	want := []string{"round-1", "heard-1", "reply-1", "replied-1", "concluded-1", "redraft-1", "redrafted-1", "round-2", "heard-2", "concluded-2"}
	if moves := f.shedMoves(t, stream); !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	// The architect's redraft is its own record of the round, not its reply,
	// and the revision it wrote is what round 2 debated.
	docs := f.documents(t, stream, shed.RedraftedDocumentID(1))
	if len(docs) != 1 || docs[0].Path != shed.RedraftedPath(1) || docs[0].Actor != architectActor {
		t.Fatalf("redraft documents %+v", docs)
	}
	report, err := shed.ParseReply([]byte(docs[0].Content))
	must(t, err)
	if report.Redraft == nil || *report.Redraft != (shed.Pin{Spec: 1, Plan: 2}) || report.Round != 1 {
		t.Fatalf("the architect's redraft %+v", report)
	}
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	i := slices.IndexFunc(records, func(r shed.Record) bool { return r.Round == 2 })
	if i < 0 || records[i].Revision != (shed.Pin{Spec: 1, Plan: 2}) {
		t.Fatalf("round 2 debated %+v", records)
	}
	// The round that debates the redraft is caused by the redraft.
	if round := f.transition(t, stream, "shed-round-2"); round.Cause != "shed-redraft-1-redrafted" || round.From != "redrafted-1" {
		t.Fatalf("round 2 %+v", round)
	}
	if asked := f.transition(t, stream, "shed-redraft-1"); asked.Cause != shed.RedraftDocumentID(1) || asked.From != "concluded-1" {
		t.Fatalf("the redraft %+v", asked)
	}
	// The conclusion of the resumed debate presents the packet again, at the
	// revision the redraft wrote.
	packet := f.awaitPacket(t, stream, "ratify: no objection stands")
	if packet.Round != 2 || packet.Revision != (shed.Pin{Spec: 1, Plan: 2}) {
		t.Fatalf("the packet of the resumed debate %+v", packet)
	}
	ratified, err := f.c.Ratify(ctx, stream, 1, 2)
	must(t, err)
	if ratified.Round != 2 || ratified.Plan != 2 || len(f.ratification(t, stream, 2).Dispositions) != 0 {
		t.Fatalf("the ratification of the redraft %+v", ratified)
	}
}

// A redraft waits for a service that can run the architect, and the owner asks
// for it once per conclusion.
func TestRedraftIsAskedForOncePerConclusionAndWaitsForARunner(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	ctx := context.Background()
	p := &faults{}
	size := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "The unit is one change.", size))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	// A redraft is refused while debate runs, so it is asked for after the
	// conclusion: a service without an architect records the request and
	// writes no redraft.
	f.stop(t)
	architect := f.opts.Architect
	f.opts.Architect = nil
	f.start(t)
	defer f.stop(t)
	out, err := f.c.ShedRedraft(ctx, stream, "Split the resume unit.")
	must(t, err)
	if out.Round != 1 {
		t.Fatalf("redraft %+v", out)
	}
	if _, err := f.c.ShedRedraft(ctx, stream, "Split it differently."); !failed(err, Conflict) || !strings.Contains(err.Error(), "already asked for") {
		t.Fatalf("asking twice after one conclusion: %v", err)
	}
	must(t, (&debate{s: f.s, repository: f.repository()}).Pass(ctx))
	if moves := f.shedMoves(t, stream); slices.Contains(moves, "redraft-1") {
		t.Fatalf("a service without an architect wrote the redraft: %v", moves)
	}
	if ran := f.ran(); ran[(roundInput{Round: 1, Redraft: true}).turnID(1)] != 0 {
		t.Fatalf("the architect ran: %v", ran)
	}
	f.opts.Architect = architect
}

// A sketched workstream whose debate is not skipped is ratified nowhere, and
// it has no packet. Without a committee it stays sketched until the owner
// skips its debate through the shed.
func TestSketchedWorkstreamIsNotRatified(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	ctx := context.Background()
	f.stop(t)
	f.opts.Committee = nil
	f.start(t)
	defer f.stop(t)
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, sketched)
	if _, err := f.c.Ratify(ctx, stream, 1, 1); !failed(err, Conflict) || !strings.Contains(err.Error(), "the workstream is sketched and debate is not skipped: it is ratified in the shed") {
		t.Fatalf("ratifying a sketched workstream: %v", err)
	}
	if _, err := f.c.Packet(ctx, stream); !failed(err, NotFound) {
		t.Fatalf("the packet of a workstream at no decision point: %v", err)
	}
	// The owner skips debate through the shed, and the chief of staff is told
	// to present the packet.
	if _, err := f.c.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.awaitPacket(t, stream, "ratify: no objection stands")
	outbox, err := f.repository().Outbox(stream)
	must(t, err)
	if !slices.ContainsFunc(outbox, func(e trace.OutboxEntry) bool {
		return e.TransitionID == skipTransition && strings.Contains(e.Event.Body, presentation("ratify: no objection stands"))
	}) {
		t.Fatalf("the chief of staff was not told of the skipped debate: %+v", outbox)
	}
}

// Debate the owner skipped at hand-in is a decision point of its own, with a
// committee configured: the packet says so, and a passing ratification asks
// for the sealing, which seals it.
func TestSkippedDebateIsRatifiedAndSeals(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	f.upstream(t)
	stream := f.handInSkipping(t, "design")

	packet := f.awaitPacket(t, stream, "ratify: no objection stands")
	if !packet.Skipped || packet.Round != 1 || packet.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(packet.Dissent) != 0 {
		t.Fatalf("the packet of a skipped debate %+v", packet)
	}
	if want := f.transition(t, stream, skipTransition).Reason; packet.Conclusion != want {
		t.Fatalf("conclusion %q, want %q", packet.Conclusion, want)
	}
	// The chief of staff is asked to present it, as it is at a conclusion.
	f.awaitFeature(t, stream, InShedState)
	if notice := f.notice(t, stream, InShedState); !strings.Contains(notice, presentation("ratify: no objection stands")) {
		t.Fatalf("the notice of the skipped debate %q", notice)
	}

	ratified, err := f.c.Ratify(ctx, stream, 1, 1)
	must(t, err)
	if ratified.Sealing != "requested" || ratified.Round != 1 || ratified.Spec != 1 || ratified.Plan != 1 {
		t.Fatalf("ratification %+v", ratified)
	}
	if record := f.ratification(t, stream, 1); record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || len(record.Dissent) != 0 {
		t.Fatalf("the ratification %+v", record)
	}
	f.awaitFeature(t, stream, BuildingState)
	if docs := f.documents(t, stream, shed.RatificationDocumentID(1)); len(docs) != 1 {
		t.Fatalf("the ratification was recorded %d times", len(docs))
	}
	f.debatedNothing(t, stream)
}

// A skip waits for the architect's redraft: the turn is dispatched and runs to
// its record, and no skipped debate leaves it unrecorded.
func TestSkipIsRefusedWhileTheRedraftRuns(t *testing.T) {
	t.Parallel()
	f := newDebateFixture(t, 1, 1)
	defer f.stop(t)
	ctx := context.Background()
	p := &faults{}
	size := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(p, "The unit is one change.", size))
	started, release := make(chan struct{}), make(chan struct{})
	redraft := roundInput{Round: 1, Redraft: true}
	f.script(redraft.turnID(1), map[string]string{plan.PlanPath: splitPlan}, held(started, release))
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	if _, err := f.c.ShedRedraft(ctx, stream, "Split the resume unit."); err != nil {
		t.Fatal(err)
	}
	awaitStart(t, started)

	_, err := f.c.ShedSkip(ctx, stream)
	if !failed(err, Conflict) || !strings.Contains(err.Error(), "is running the redraft after round 1; skip debate once it is recorded") {
		t.Fatalf("skipping a running redraft: %v", err)
	}
	close(release)
	f.awaitShed(t, stream, "redrafted-1")
	p.check(t)
	// The redraft the skip waited for is on the record, with the revision it
	// wrote.
	docs := f.documents(t, stream, shed.RedraftedDocumentID(1))
	if len(docs) != 1 {
		t.Fatalf("redraft documents %+v", docs)
	}
	report, err := shed.ParseReply([]byte(docs[0].Content))
	must(t, err)
	if report.Redraft == nil || *report.Redraft != (shed.Pin{Spec: 1, Plan: 2}) {
		t.Fatalf("the architect's redraft %+v", report)
	}
}

// The conclusion at the round limit names what stopped the debate: the
// configured cap, the further rounds the owner asked for, or the round that
// debated the redraft they asked for instead.
func TestBoundNamesWhatStoppedTheDebate(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		requests []shed.More
		redrafts []shed.Redraft
		limit    int
		want     string
	}{
		"the configured cap":          {nil, nil, 3, "at the shed.max_rounds cap of 3"},
		"the rounds asked for":        {[]shed.More{{Round: 1, Rounds: 1}}, nil, 2, "at round 2, the last of the further rounds the owner asked for"},
		"the redraft asked for":       {nil, []shed.Redraft{{Round: 2}}, 3, "at round 3, the round that debated the redraft the owner asked for"},
		"the later of the two":        {[]shed.More{{Round: 1, Rounds: 1}}, []shed.Redraft{{Round: 3}}, 4, "at round 4, the round that debated the redraft the owner asked for"},
		"the rounds asked for, later": {[]shed.More{{Round: 3, Rounds: 2}}, []shed.Redraft{{Round: 1}}, 5, "at round 5, the last of the further rounds the owner asked for"},
	} {
		if got := bound(3, tc.requests, tc.redrafts, tc.limit); got != tc.want {
			t.Fatalf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
