package intakejourney

import (
	context "context"
	fmt "fmt"
	reflect "reflect"
	slices "slices"
	strings "strings"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	plan "github.com/kpenfound/osmia/internal/plan"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPacketPresentsTheDecisionAndTheOverruleUnblocksIt(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 1)
	defer f.Stop(t)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	veto := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	advice := shed.ObjectionID(1, intake.CommitteeAgent(2), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Charter, "spec#2", "charter#1"))
	f.Member(1, 2, 1, intakefixture.Objects(p, shed.Fit, "plan", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(p, "Kept as it is.", veto, advice))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)

	blocked := fmt.Sprintf("do not ratify yet: ratification is blocked by 1 objection (%s); overrule or sustain each one, or ask for a redraft", veto)
	packet := f.AwaitPacket(t, stream, blocked)
	conclusion := f.Transition(t, stream, "shed-concluded-1").Reason
	if packet.Round != 1 || packet.Revision != (shed.Pin{Spec: 1, Plan: 1}) || packet.Skipped || packet.Conclusion != conclusion {
		t.Fatalf("packet %+v", packet)
	}

	if len(packet.Dissent) != 2 || packet.Dissent[0].ID != veto || !packet.Dissent[0].Blocking || packet.Dissent[1].ID != advice || packet.Dissent[1].Blocking {
		t.Fatalf("the packet's dissent record %+v", packet.Dissent)
	}
	docs := f.Documents(t, stream, shed.PacketDocumentID(1))
	if len(docs) != 1 || docs[0].Path != shed.PacketPath(1) || docs[0].Actor != intake.ShedActor || docs[0].Cause != "ratification-packet" {
		t.Fatalf("packet documents %+v", docs)
	}

	if notice := f.Concluded(t, stream, 1).Event.Body; !strings.Contains(notice, intake.Presentation(blocked)+intake.DecideFirst(packet.Dissent)) {
		t.Fatalf("notice %q, want %q", notice, intake.Presentation(blocked)+intake.DecideFirst(packet.Dissent))
	}
	if entries := f.InboxEntries(t, stream, api.InboxRatification); len(entries) != 0 {
		t.Fatalf("inbox entries before the manager decided %+v", entries)
	}
	f.ManagerDecides(t, stream, veto, shed.Escalate, "The veto cites a rule the plan may break.")
	f.ManagerDecides(t, stream, advice, shed.Escalate, "The plan order is a judgement call.")
	_, presented := f.AwaitDecided(t, stream)

	if entries := f.InboxEntries(t, stream, api.InboxRatification); len(entries) != 1 || entries[0].Revision != presented || entries[0].Recommendation != blocked || entries[0].Options == nil || len(entries[0].Options) != 0 ||
		!reflect.DeepEqual(entries[0].Answer, api.InboxAnswer{Method: "POST", Path: "/v1/ratify/" + string(stream), Body: map[string]any{"spec": 1.0, "plan": 1.0}}) {
		t.Fatalf("inbox entries of the packet %+v", entries)
	}

	_, err := f.C.Ratify(ctx, stream, 1, 1)
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), fmt.Sprintf("objection %s (charter, by %s in round 1 on spec#2) blocks and has no disposition", veto, intake.CommitteeAgent(1))) {
		t.Fatalf("ratifying over a charter veto: %v", err)
	}

	out, err := f.C.ShedOverrule(ctx, stream, veto, "I accept the risk.")
	fixturekit.Must(t, err)
	if out.Action != string(shed.Overruled) || out.Objection != veto || out.Round != 1 || !strings.Contains(out.Detail, "I accept the risk.") {
		t.Fatalf("overrule %+v", out)
	}
	if moves := f.OwnerMoves(t, stream); !slices.Equal(moves, []string{"overruled-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	entries := f.Dissent(t, stream)
	if len(entries) != 2 || entries[0].ID != veto || entries[0].Blocking || entries[0].Disposition != shed.Overruled || entries[0].Note != "I accept the risk." {
		t.Fatalf("dissent after the overrule %+v", entries)
	}

	ratifiable := "ratify: nothing blocks, and 2 objections stand as advice on the record"
	if packet := f.AwaitPacket(t, stream, ratifiable); len(packet.Dissent) != 2 || packet.Dissent[0].Disposition != shed.Overruled {
		t.Fatalf("the packet after the overrule %+v", packet)
	}
	if docs := f.Documents(t, stream, shed.PacketDocumentID(1)); len(docs) != presented+1 || docs[presented].Revision != presented+1 {
		t.Fatalf("packet documents after the overrule %+v", docs)
	}

	if entries := f.InboxEntries(t, stream, api.InboxRatification); len(entries) != 1 || entries[0].Revision != presented+1 || entries[0].Recommendation != ratifiable || !slices.Equal(entries[0].Options, []string{"ratify"}) {
		t.Fatalf("inbox entries after the overrule %+v", entries)
	}

	if _, err := f.C.ShedRule(ctx, stream, veto, "sustain", "On reflection, no."); err != nil {
		t.Fatal(err)
	}
	f.AwaitPacket(t, stream, blocked)
	if _, err := f.C.ShedOverrule(ctx, stream, veto, "I accept the risk."); err != nil {
		t.Fatal(err)
	}
	f.AwaitPacket(t, stream, ratifiable)
	docs = f.Documents(t, stream, shed.PacketDocumentID(1))
	if len(docs) != presented+3 || docs[presented+2].Revision != presented+3 || docs[presented+2].Content != docs[presented].Content {
		t.Fatalf("packet documents after the owner changed their mind twice %+v", docs)
	}

	ratified, err := f.C.Ratify(ctx, stream, 1, 1)
	fixturekit.Must(t, err)
	if ratified.Round != 1 || ratified.Spec != 1 || ratified.Plan != 1 || ratified.Sealing != "requested" || ratified.Workstream != stream {
		t.Fatalf("ratification %+v", ratified)
	}
	if want := "the owner ratified spec.md revision 1 and plan.json revision 1 after round 1, over 1 objection the owner disposed of; the sealing is asked for"; ratified.Detail != want {
		t.Fatalf("detail %q, want %q", ratified.Detail, want)
	}
	if entries := f.InboxEntries(t, stream, api.InboxRatification); len(entries) != 0 {
		t.Fatalf("inbox entries after the ratification %+v", entries)
	}
	record := f.Ratification(t, stream, 1)
	if record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || record.Round != 1 || len(record.Dispositions) != 1 ||
		record.Dispositions[0] != (shed.Ruling{Objection: veto, Disposition: shed.Overruled, Note: "I accept the risk."}) || len(record.Dissent) != 2 {
		t.Fatalf("the recorded ratification %+v", record)
	}
	if moves := f.OwnerMoves(t, stream); !slices.Equal(moves, []string{"overruled-1", "ruled-1", "overruled-1", "ratified-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}

	f.StillInShed(t, stream)

	again, err := f.C.Ratify(ctx, stream, 1, 1)
	fixturekit.Must(t, err)
	if !slices.Contains([]string{"requested", "pending", "running"}, again.Sealing) || again.Round != 1 || !strings.HasPrefix(again.Detail, fmt.Sprintf("workstream %s is ratified at %s already; ", stream, shed.Pin{Spec: 1, Plan: 1})) {
		t.Fatalf("ratifying twice %+v", again)
	}
	if docs := f.Documents(t, stream, shed.RatificationDocumentID(1)); len(docs) != 1 {
		t.Fatalf("ratifying twice recorded %+v", docs)
	}
	if moves := f.OwnerMoves(t, stream); !slices.Equal(moves, []string{"overruled-1", "ruled-1", "overruled-1", "ratified-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	p.Check(t)
}

func TestRatifyListsEveryReasonItIsRefused(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Charter, "plan#resume", "charter#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(p, "The unit is one change.", size))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	f.AwaitPacket(t, stream, shed.Recommend(f.Dissent(t, stream)))

	fixturekit.Must(t, f.Repository().RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: plan.SpecDocument, Revision: 2,
		Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.ArchitectActor, Cause: "test"},
		Path: plan.SpecPath, Content: fixturekit.ValidSpec + "3. Nobody planned this one.\n"}}))

	_, err := f.C.Ratify(ctx, stream, 1, 1)
	if !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("ratifying a stale revision of an invalid plan over an objection: %v", err)
	}
	for _, want := range []string{
		"spec.md revision 1 and plan.json revision 1 are ratified, and the current revisions are spec.md revision 2 and plan.json revision 1: read the packet again",
		fmt.Sprintf("objection %s (charter, by %s in round 1 on plan#resume) blocks and has no disposition", size, intake.CommitteeAgent(1)),
		"the plan is not valid: spec#3",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal lacks %q:\n%v", want, err)
		}
	}

	if _, err := f.C.ShedRule(ctx, stream, size, "sustain", "Split it."); err != nil {
		t.Fatal(err)
	}
	_, err = f.C.Ratify(ctx, stream, 2, 1)
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), fmt.Sprintf("objection %s (charter, by %s in round 1 on plan#resume) blocks and is sustained and is not conceded", size, intake.CommitteeAgent(1))) {
		t.Fatalf("ratifying over a sustained objection: %v", err)
	}

	for name, req := range map[string]api.RatifyRequest{"no spec": {Plan: 1}, "no plan": {Spec: 1}, "a negative revision": {Spec: 1, Plan: -1}} {
		if _, err := f.C.Ratify(ctx, stream, req.Spec, req.Plan); !intakefixture.Failed(err, api.Validation) {
			t.Fatalf("ratifying with %s: %v", name, err)
		}
	}
}

func TestRedraftGoesBackToTheArchitectAndDebateResumes(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(p, "The unit is one change.", size))

	redraft := intake.RoundInput{Round: 1, Redraft: true}
	f.Script(redraft.TurnID(1), map[string]string{plan.PlanPath: splitPlan}, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		for _, want := range []string{"the owner read the packet and asked you to redraft spec.md revision 1 and plan.json revision 1", "Split the resume unit into what it addresses.",
			fmt.Sprintf("- %s (size, advisory, by %s in round 1 on plan#resume, citing spec#1): It does not hold.", size, intake.CommitteeAgent(1))} {
			if !strings.Contains(req.Prompt, want) {
				p.Report("the redraft prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		return nil
	})
	f.Member(2, 1, 1, intakefixture.Concedes(p, size))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")

	if _, err := f.C.ShedRedraft(ctx, stream, "  "); !intakefixture.Failed(err, api.Validation) {
		t.Fatalf("a redraft without a note: %v", err)
	}
	out, err := f.C.ShedRedraft(ctx, stream, "Split the resume unit into what it addresses.")
	fixturekit.Must(t, err)
	if out.Action != "redraft" || out.Round != 1 || !strings.Contains(out.Detail, "Split the resume unit into what it addresses.") {
		t.Fatalf("redraft %+v", out)
	}
	asked, err := shed.Redrafts(f.Repository(), stream)
	fixturekit.Must(t, err)
	if len(asked) != 1 || asked[0].Round != 1 || asked[0].Revision != (shed.Pin{Spec: 1, Plan: 1}) {
		t.Fatalf("the recorded request %+v", asked)
	}

	f.AwaitShed(t, stream, "concluded-2", "concluded-3")
	p.Check(t)
	want := []string{"round-1", "heard-1", "reply-1", "replied-1", "concluded-1", "redraft-1", "redrafted-1", "round-2", "heard-2", "concluded-2"}
	if moves := f.ShedMoves(t, stream); !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}

	docs := f.Documents(t, stream, shed.RedraftedDocumentID(1))
	if len(docs) != 1 || docs[0].Path != shed.RedraftedPath(1) || docs[0].Actor != intake.ArchitectActor {
		t.Fatalf("redraft documents %+v", docs)
	}
	report, err := shed.ParseReply([]byte(docs[0].Content))
	fixturekit.Must(t, err)
	if report.Redraft == nil || *report.Redraft != (shed.Pin{Spec: 1, Plan: 2}) || report.Round != 1 {
		t.Fatalf("the architect's redraft %+v", report)
	}
	records, err := shed.Records(f.Repository(), stream)
	fixturekit.Must(t, err)
	i := slices.IndexFunc(records, func(r shed.Record) bool { return r.Round == 2 })
	if i < 0 || records[i].Revision != (shed.Pin{Spec: 1, Plan: 2}) {
		t.Fatalf("round 2 debated %+v", records)
	}

	if round := f.Transition(t, stream, "shed-round-2"); round.Cause != "shed-redraft-1-redrafted" || round.From != "redrafted-1" {
		t.Fatalf("round 2 %+v", round)
	}
	if asked := f.Transition(t, stream, "shed-redraft-1"); asked.Cause != shed.RedraftDocumentID(1) || asked.From != "concluded-1" {
		t.Fatalf("the redraft %+v", asked)
	}

	packet := f.AwaitPacket(t, stream, "ratify: no objection stands")
	if packet.Round != 2 || packet.Revision != (shed.Pin{Spec: 1, Plan: 2}) {
		t.Fatalf("the packet of the resumed debate %+v", packet)
	}
	ratified, err := f.C.Ratify(ctx, stream, 1, 2)
	fixturekit.Must(t, err)
	if ratified.Round != 2 || ratified.Plan != 2 || len(f.Ratification(t, stream, 2).Dispositions) != 0 {
		t.Fatalf("the ratification of the redraft %+v", ratified)
	}
}

func TestRedraftIsAskedForOncePerConclusionAndWaitsForARunner(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(p, "The unit is one change.", size))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)

	f.Stop(t)
	architect := f.Opts.Architect
	f.Opts.Architect = nil
	f.Start(t)
	defer f.Stop(t)
	out, err := f.C.ShedRedraft(ctx, stream, "Split the resume unit.")
	fixturekit.Must(t, err)
	if out.Round != 1 {
		t.Fatalf("redraft %+v", out)
	}
	if _, err := f.C.ShedRedraft(ctx, stream, "Split it differently."); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "already asked for") {
		t.Fatalf("asking twice after one conclusion: %v", err)
	}
	repo := f.Repository()
	drivePass(t, f.S, repo, func() error {
		return (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}).Pass(ctx)
	})
	if moves := f.ShedMoves(t, stream); slices.Contains(moves, "redraft-1") {
		t.Fatalf("a service without an architect wrote the redraft: %v", moves)
	}
	if ran := f.Ran(); ran[(intake.RoundInput{Round: 1, Redraft: true}).TurnID(1)] != 0 {
		t.Fatalf("the architect ran: %v", ran)
	}
	f.Opts.Architect = architect
}

func TestSketchedWorkstreamIsNotRatified(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	ctx := context.Background()
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	defer f.Stop(t)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)
	if _, err := f.C.Ratify(ctx, stream, 1, 1); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "the workstream is sketched and debate is not skipped: it is ratified in the shed") {
		t.Fatalf("ratifying a sketched workstream: %v", err)
	}
	if _, err := f.C.Packet(ctx, stream); !intakefixture.Failed(err, api.NotFound) {
		t.Fatalf("the packet of a workstream at no decision point: %v", err)
	}

	if _, err := f.C.ShedSkip(ctx, stream); err != nil {
		t.Fatal(err)
	}
	f.AwaitPacket(t, stream, "ratify: no objection stands")
	outbox, err := f.Repository().Outbox(stream)
	fixturekit.Must(t, err)
	if !slices.ContainsFunc(outbox, func(e trace.OutboxEntry) bool {
		return e.TransitionID == intake.SkipTransition && strings.Contains(e.Event.Body, intake.Presentation("ratify: no objection stands"))
	}) {
		t.Fatalf("the manager was not told of the skipped debate: %+v", outbox)
	}
	content := fixturekit.HandedDesign
	out, err := f.C.HandIn(ctx, api.HandInRequest{Project: f.Project, Key: "design", Stdin: &content})
	fixturekit.Must(t, err)
	if out.Workstream != stream || out.SkipDebate {
		t.Fatalf("retry %+v", out)
	}
	_, err = f.C.HandIn(ctx, api.HandInRequest{Project: f.Project, Key: "design", Stdin: &content, SkipDebate: true})
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "key design already handed in workstream "+string(stream)+" without skipping debate; use a new key") {
		t.Fatalf("retry with the skip: %v", err)
	}
}

func TestSkipIsRefusedWhileTheRedraftRuns(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(p, "The unit is one change.", size))
	started, release := make(chan struct{}), make(chan struct{})
	redraft := intake.RoundInput{Round: 1, Redraft: true}
	f.Script(redraft.TurnID(1), map[string]string{plan.PlanPath: splitPlan}, intakefixture.Held(started, release))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	if _, err := f.C.ShedRedraft(ctx, stream, "Split the resume unit."); err != nil {
		t.Fatal(err)
	}
	intakefixture.AwaitStart(t, started)

	_, err := f.C.ShedSkip(ctx, stream)
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "is running the redraft after round 1; skip debate once it is recorded") {
		t.Fatalf("skipping a running redraft: %v", err)
	}
	close(release)
	f.AwaitShed(t, stream, "redrafted-1")
	p.Check(t)

	docs := f.Documents(t, stream, shed.RedraftedDocumentID(1))
	if len(docs) != 1 {
		t.Fatalf("redraft documents %+v", docs)
	}
	report, err := shed.ParseReply([]byte(docs[0].Content))
	fixturekit.Must(t, err)
	if report.Redraft == nil || *report.Redraft != (shed.Pin{Spec: 1, Plan: 2}) {
		t.Fatalf("the architect's redraft %+v", report)
	}
}
