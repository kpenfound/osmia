package intakejourney

import (
	context "context"
	fmt "fmt"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestOwnerObjectionIsAnsweredLikeAMembers(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	started, release := make(chan struct{}), make(chan struct{})
	f.Member(1, 1, 1, intakefixture.Held(started, release))
	var answered []string
	f.Script(intake.ReplyTurnID(1, 1), nil, func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {

		if want := "- owner-r1-1 (owner, blocking, by owner in round 1): The plan never names the retry budget.\n"; !strings.Contains(req.Prompt, want) {
			p.Report("the reply prompt lacks %q:\n%s", want, req.Prompt)
		}
		if recorded, reason, err := intakefixture.ShedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": "owner-r1-1", "answer": "Named in unit resume."}); err != nil || !recorded {
			p.Report("answer to the owner: %q %v", reason, err)
		} else {
			answered = append(answered, "owner-r1-1")
		}
		return nil
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	intakefixture.AwaitStart(t, started)

	out, err := f.C.ShedObject(ctx, stream, "The plan never names the retry budget.")
	fixturekit.Must(t, err)
	if out.Objection != "owner-r1-1" || out.Round != 1 || out.Action != "objected" || out.Workstream != stream || out.Project != f.Project {
		t.Fatalf("objection %+v", out)
	}
	record := f.OwnerRecord(t, stream, 1)
	if len(record.Objections) != 1 || record.Objections[0].Kind != shed.Owner || record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || record.Objections[0].Argument != "The plan never names the retry budget." {
		t.Fatalf("owner record %+v", record)
	}
	if moves := f.OwnerMoves(t, stream); !slices.Equal(moves, []string{"objected-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	close(release)

	f.AwaitShed(t, stream, "concluded-1")
	entries := f.Dissent(t, stream)
	if len(entries) != 1 || entries[0].ID != "owner-r1-1" || !entries[0].Blocking || entries[0].Member != shed.OwnerMember || entries[0].Round != 1 {
		t.Fatalf("dissent %+v", entries)
	}
	if len(answered) != 1 {
		t.Fatalf("the architect answered %v", answered)
	}
	reply := f.Reply(t, stream, 1)
	if len(reply.Answers) != 1 || reply.Answers[0].Objection != "owner-r1-1" {
		t.Fatalf("reply %+v", reply)
	}

	docs := f.Documents(t, stream, shed.DocumentID(1, intake.CommitteeAgent(1)))
	if len(docs) != 1 {
		t.Fatalf("the round recorded %d member files", len(docs))
	}
	heard, err := shed.Parse([]byte(docs[0].Content))
	fixturekit.Must(t, err)
	if !heard.Silent() || heard.Owned() {
		t.Fatalf("member record %+v", heard)
	}
	if reason := f.Transition(t, stream, "shed-round-1-heard").Reason; !strings.Contains(reason, "1 members heard, 0 objections") {
		t.Fatalf("round 1 %q", reason)
	}
	p.Check(t)

	second, err := f.C.ShedObject(ctx, stream, "The proof is a manual check.")
	fixturekit.Must(t, err)
	if second.Objection != "owner-r1-2" {
		t.Fatalf("second objection %+v", second)
	}
	if record := f.OwnerRecord(t, stream, 1); len(record.Objections) != 2 {
		t.Fatalf("owner record %+v", record)
	}
	if got := len(f.Dissent(t, stream)); got != 2 {
		t.Fatalf("dissent entries %d", got)
	}
	if _, err := f.C.ShedObject(ctx, stream, "  "); !intakefixture.Failed(err, api.Validation) {
		t.Fatalf("empty argument: %v", err)
	}
}

func TestAnAbandonedRoundFailsThoughTheOwnerObjected(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	started := make(chan struct{})
	f.Member(1, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	intakefixture.AwaitStart(t, started)
	if _, err := f.C.ShedObject(ctx, stream, "The plan never names the retry budget."); err != nil {
		t.Fatal(err)
	}
	if _, err := f.C.Abandon(ctx, stream, "no longer needed"); err != nil {
		t.Fatal(err)
	}
	f.AwaitShed(t, stream, "failed-1", "heard-1")
	records, err := shed.Records(f.Repository(), stream)
	fixturekit.Must(t, err)
	if len(records) != 1 || !records[0].Owned() {
		t.Fatalf("records of an abandoned round: %+v", records)
	}
	if reason := f.Transition(t, stream, "shed-round-1-failed").Reason; !strings.Contains(reason, "the committee is not heard") {
		t.Fatalf("failure %q", reason)
	}
	ops := f.AcknowledgedRoundOperations(t, stream)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || !strings.Contains(ops[0].Result.Evidence, "the workstream was abandoned") {
		t.Fatalf("operations: %+v", ops)
	}
}

func TestOwnerRulingDisposesOfAnObjection(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 1)
	defer f.Stop(t)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	fit := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	veto := shed.ObjectionID(1, intake.CommitteeAgent(2), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Fit, "spec#1", "spec#1"))
	f.Member(1, 2, 1, intakefixture.Objects(p, shed.Charter, "plan#resume", "charter#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(p, "Kept as it is.", fit, veto))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)

	out, err := f.C.ShedRule(ctx, stream, veto, "dismiss", "I accept the risk.")
	fixturekit.Must(t, err)
	if out.Action != string(shed.Dismissed) || out.Objection != veto || out.Round != 1 || !strings.Contains(out.Detail, "I accept the risk.") {
		t.Fatalf("ruling %+v", out)
	}
	if _, err := f.C.ShedRule(ctx, stream, fit, "sustain", ""); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		blocking    bool
		disposition shed.Disposition
	}{fit: {true, shed.Sustained}, veto: {false, shed.Dismissed}}
	entries := f.Dissent(t, stream)
	if len(entries) != 2 {
		t.Fatalf("dissent %+v", entries)
	}
	for _, e := range entries {
		if e.Blocking != want[e.ID].blocking || e.Disposition != want[e.ID].disposition {
			t.Fatalf("entry %+v, want %+v", e, want[e.ID])
		}
	}
	if moves := f.OwnerMoves(t, stream); !slices.Equal(moves, []string{"ruled-1", "ruled-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}

	if _, err := f.C.ShedRule(ctx, stream, veto, "sustain", "I changed my mind."); err != nil {
		t.Fatal(err)
	}
	docs := f.Documents(t, stream, shed.RulingsDocumentID(1))
	if len(docs) != 3 || docs[2].Revision != 3 || docs[2].Path != shed.RulingsPath(1) || docs[2].Actor != intake.OwnerActor {
		t.Fatalf("rulings documents %+v", docs)
	}
	rulings, err := shed.ParseRulings([]byte(docs[2].Content))
	fixturekit.Must(t, err)
	if len(rulings.Rulings) != 2 {
		t.Fatalf("rulings %+v", rulings)
	}
	for _, e := range f.Dissent(t, stream) {
		if !e.Blocking || e.Disposition != shed.Sustained {
			t.Fatalf("entry after the second ruling %+v", e)
		}
	}
	for name, tc := range map[string]struct {
		objection, disposition string
		code                   api.Code
	}{
		"an objection that does not stand": {"agent_committee_1-r1-9", "dismiss", api.NotFound},
		"an unknown disposition":           {fit, "overrule", api.Validation},
		"the recorded disposition":         {fit, string(shed.Sustained), api.Validation},
	} {
		if _, err := f.C.ShedRule(ctx, stream, tc.objection, tc.disposition, ""); !intakefixture.Failed(err, tc.code) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestDismissingEveryObjectionConcludesTheDebate(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 2)
	defer f.Stop(t)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	objection := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	replying, release := make(chan struct{}), make(chan struct{})
	f.Script(intake.ReplyTurnID(1, 1), nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(replying)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	select {
	case <-replying:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the reply did not start")
	}
	if _, err := f.C.ShedRule(ctx, stream, objection, "dismiss", "The unit is small enough."); err != nil {
		t.Fatal(err)
	}
	close(release)

	f.AwaitShed(t, stream, "concluded-1", "round-2")
	end := f.Transition(t, stream, "shed-concluded-1")
	if end.Reason != "debate concluded after round 1: the owner disposed of every objection that stood" {
		t.Fatalf("conclusion %q", end.Reason)
	}

	entries := f.Dissent(t, stream)
	if len(entries) != 1 || entries[0].ID != objection || entries[0].Blocking || entries[0].Disposition != shed.Dismissed {
		t.Fatalf("dissent %+v", entries)
	}
	line := fmt.Sprintf("\n- %s (size, dismissed, advisory, by %s in round 1 on plan#resume, against %s): It does not hold.", objection, intake.CommitteeAgent(1), shed.Pin{Spec: 1, Plan: 1})
	if notice := f.Concluded(t, stream, 1); !strings.Contains(notice.Event.Body, "Open dissent:"+line) {
		t.Fatalf("notice %q, want a line %q", notice.Event.Body, line)
	}
	if ran := f.Ran(); ran[intake.RoundTurnID(2, intake.CommitteeAgent(1), 1)] != 0 {
		t.Fatalf("round 2 ran: %v", ran)
	}
	p.Check(t)
}

func TestOwnerSkipsDebateAcrossARestart(t *testing.T) {
	t.Parallel()
	passed, prepare := journey.CountingSchedule()
	f := journey.NewDebateFixtureWith(t, 1, 1, "", prepare)
	ctx := context.Background()
	runner := f.Opts.Committee
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)

	out, err := f.C.ShedSkip(ctx, stream)
	fixturekit.Must(t, err)
	if out.Action != intake.SkippedValue || out.Workstream != stream || !strings.Contains(out.Detail, "ratification") {
		t.Fatalf("skip %+v", out)
	}
	if _, err := f.C.ShedSkip(ctx, stream); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("skipping twice: %v", err)
	}

	if state, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != intake.InShedState {
		t.Fatalf("feature %+v %v", state, err)
	}
	if threads, err := f.Repository().Threads(stream); err == nil {
		for _, th := range threads {
			if th.Identity.Role == intake.CommitteeRole {
				t.Fatalf("a committee thread was created: %+v", th.Identity)
			}
		}
	}

	objection := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	record := shed.Record{Version: shed.Version, Round: 1, Member: intake.CommitteeAgent(1), Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: intake.RoundTurnID(1, intake.CommitteeAgent(1), 1),
		Objections: []shed.Objection{{ID: objection, Kind: shed.Size, Part: "plan#resume", Argument: "It does too much.", Citations: []string{"spec#1"}}}}
	content, err := shed.Encode(record)
	fixturekit.Must(t, err)
	fixturekit.Must(t, f.Repository().RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.DocumentID(1, intake.CommitteeAgent(1)), Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: trace.Actor{Kind: "agent", ID: intake.CommitteeAgent(1)}, Cause: "planted"}, Path: shed.Path(1, intake.CommitteeAgent(1)), Content: string(content)}}))
	if _, err := f.C.ShedRule(ctx, stream, objection, "dismiss", "Small enough."); err != nil {
		t.Fatal(err)
	}
	f.Stop(t)

	f.Opts.Committee = runner
	f.Start(t)
	defer f.Stop(t)
	intakefixture.AwaitPasses(t, passed, 3)
	if moves := f.ShedMoves(t, stream); len(moves) != 0 || len(f.RoundOperations(t, stream)) != 0 {
		t.Fatalf("a skipped debate ran %v", moves)
	}
	if ran := f.Ran(); ran[intake.RoundTurnID(1, intake.CommitteeAgent(1), 1)] != 0 {
		t.Fatalf("a member ran: %v", ran)
	}
	threads, err := f.Repository().Threads(stream)
	fixturekit.Must(t, err)
	if slices.ContainsFunc(threads, func(th trace.Thread) bool { return th.Identity.Role == intake.CommitteeRole }) {
		t.Fatalf("a committee was created for a skipped debate: %+v", threads)
	}
	if moves, want := f.OwnerMoves(t, stream), []string{intake.SkippedValue, "ruled-1"}; !slices.Equal(moves, want) {
		t.Fatalf("owner subject went %v, want %v", moves, want)
	}
	f.StillInShed(t, stream)

	if _, err := f.C.ShedObject(ctx, stream, "Too late."); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("objecting after a skip: %v", err)
	}
	if _, err := f.C.ShedMore(ctx, stream, 1); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("more after a skip: %v", err)
	}
	_, err = f.C.ShedRedraft(ctx, stream, "Split it.")
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "skipped debate") {
		t.Fatalf("a redraft after a skip: %v", err)
	}
}

func TestSkipIsRefusedWhileARoundRunsAndOutsideTheShed(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	started, release := make(chan struct{}), make(chan struct{})
	f.Member(1, 1, 1, intakefixture.Held(started, release))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	intakefixture.AwaitStart(t, started)
	if _, err := f.C.ShedSkip(ctx, stream); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("skipping a running round: %v", err)
	}

	_, err := f.C.ShedMore(ctx, stream, 1)
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "has not concluded") {
		t.Fatalf("more while round 1 runs: %v", err)
	}
	_, err = f.C.ShedRedraft(ctx, stream, "Split it.")
	if !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), "has not concluded") {
		t.Fatalf("a redraft while round 1 runs: %v", err)
	}
	close(release)
	f.AwaitShed(t, stream, "concluded-1")

	if _, err := f.C.Abandon(ctx, stream, "Superseded."); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"skip":     func() error { _, err := f.C.ShedSkip(ctx, stream); return err },
		"object":   func() error { _, err := f.C.ShedObject(ctx, stream, "No."); return err },
		"rule":     func() error { _, err := f.C.ShedRule(ctx, stream, "owner-r1-1", "dismiss", ""); return err },
		"overrule": func() error { _, err := f.C.ShedOverrule(ctx, stream, "owner-r1-1", ""); return err },
		"more":     func() error { _, err := f.C.ShedMore(ctx, stream, 1); return err },
		"redraft":  func() error { _, err := f.C.ShedRedraft(ctx, stream, "Split it."); return err },
		"ratify":   func() error { _, err := f.C.Ratify(ctx, stream, 1, 1); return err },
	} {
		if err := call(); !intakefixture.Failed(err, api.Conflict) || !strings.Contains(err.Error(), intake.AbandonedState) {
			t.Fatalf("%s on an abandoned workstream: %v", name, err)
		}
	}
}

func TestOwnerAsksForMoreRoundsAcrossARestart(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	objection := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(p, "Kept as one unit.", objection))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)

	for name, tc := range map[string]struct {
		rounds int
		code   api.Code
	}{"none": {0, api.Validation}, "negative": {-1, api.Validation}, "beyond the cap": {2, api.Validation}} {
		if _, err := f.C.ShedMore(ctx, stream, tc.rounds); !intakefixture.Failed(err, tc.code) {
			t.Fatalf("%s: %v", name, err)
		}
	}

	runner := f.Opts.Committee
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	out, err := f.C.ShedMore(ctx, stream, 1)
	fixturekit.Must(t, err)
	if out.Action != "more" || out.Rounds != 1 || out.Round != 1 {
		t.Fatalf("more %+v", out)
	}

	if _, err := f.C.ShedMore(ctx, stream, 1); err != nil {
		t.Fatal(err)
	}
	if docs := f.Documents(t, stream, shed.MoreDocumentID(1)); len(docs) != 2 || docs[1].Revision != 2 || docs[1].Path != shed.MorePath(1) {
		t.Fatalf("requests %+v", docs)
	}

	repo := f.Repository()
	drivePass(t, f.S, repo, func() error {
		return (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}).Pass(ctx)
	})
	if moves := f.ShedMoves(t, stream); moves[len(moves)-1] != "concluded-1" {
		t.Fatalf("a round ran without a committee runner: %v", moves)
	}
	f.Stop(t)

	f.Opts.Committee = runner
	f.Script(intake.ReplyTurnID(2, 1), nil, nil)
	f.Member(2, 1, 1, intakefixture.Concedes(p, objection))
	f.Start(t)
	defer f.Stop(t)
	f.AwaitShed(t, stream, "concluded-2")
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "concluded-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	resumed := f.Transition(t, stream, "shed-round-2")
	if resumed.From != "concluded-1" || resumed.Cause != shed.MoreDocumentID(1) {
		t.Fatalf("resumed round %+v", resumed)
	}
	if entries := f.Dissent(t, stream); len(entries) != 0 {
		t.Fatalf("dissent after the concession %+v", entries)
	}

	repo = f.Repository()
	drivePass(t, f.S, repo, func() error {
		return (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}).Pass(ctx)
	})
	if moves := f.ShedMoves(t, stream); moves[len(moves)-1] != "concluded-2" {
		t.Fatalf("the debate resumed without a request: %v", moves)
	}
	p.Check(t)
}

func TestShedActionsRefuseAnUnknownWorkstream(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	ctx := context.Background()
	unknown := config.WorkstreamID("w_00000000000000000000000000000009")
	calls := map[string]func(config.WorkstreamID) error{
		"object": func(id config.WorkstreamID) error { _, err := f.C.ShedObject(ctx, id, "No."); return err },
		"rule": func(id config.WorkstreamID) error {
			_, err := f.C.ShedRule(ctx, id, "owner-r1-1", "dismiss", "")
			return err
		},
		"overrule": func(id config.WorkstreamID) error {
			_, err := f.C.ShedOverrule(ctx, id, "owner-r1-1", "")
			return err
		},
		"skip":    func(id config.WorkstreamID) error { _, err := f.C.ShedSkip(ctx, id); return err },
		"more":    func(id config.WorkstreamID) error { _, err := f.C.ShedMore(ctx, id, 1); return err },
		"redraft": func(id config.WorkstreamID) error { _, err := f.C.ShedRedraft(ctx, id, "Split it."); return err },
		"packet":  func(id config.WorkstreamID) error { _, err := f.C.Packet(ctx, id); return err },
		"ratify":  func(id config.WorkstreamID) error { _, err := f.C.Ratify(ctx, id, 1, 1); return err },
	}
	for name, call := range calls {
		if err := call(unknown); !intakefixture.Failed(err, api.Validation) {
			t.Fatalf("%s on an unknown workstream: %v", name, err)
		}
	}
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1", fmt.Sprintf("failed-%d", 1))
	_, err := f.C.Abandon(ctx, stream, "Owner finished checking shed actions")
	fixturekit.Must(t, err)
	if _, err := f.C.RemoveProject(ctx, f.Project); err != nil {
		t.Fatal(err)
	}
	for name, call := range calls {
		if err := call(stream); !intakefixture.Failed(err, api.NoProject) {
			t.Fatalf("%s with no project: %v", name, err)
		}
	}
}
