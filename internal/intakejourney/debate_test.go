package intakejourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/projectstore"
	questions "github.com/kpenfound/osmia/internal/questions"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	splitPlan = `{"version": 2, "units": [
  {"id": "resume-read", "title": "Find the last chunk", "task": "Find the last chunk", "acceptance": ["spec#1 holds, shown by TestLastChunk (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "resume-write", "title": "Resume from it", "task": "Resume from it", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": ["resume-read"], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "task": "Skip acknowledged chunks", "acceptance": ["spec#2 holds, shown by TestNoChunkTwice (new-test)"], "criteria": ["spec#2"], "depends_on": ["resume-write"], "footprint": ["internal.trace"]}
]}
`
	cycleProblem = `unit "dedupe": dependency cycle dedupe -> resume -> dedupe`
)

func TestDebateConcludesByConsensusInOneRound(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 3)
	defer f.Stop(t)
	f.Member(1, 1, 1, intakefixture.Silent)
	f.Member(1, 2, 1, intakefixture.Silent)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	end := f.Transition(t, stream, "shed-concluded-1")
	if end.Reason != "debate concluded by consensus after round 1: no objection stands" || end.Cause != "shed-round-1-heard" || end.Actor != intake.ShedActor || end.From != "heard-1" {
		t.Fatalf("conclusion: %+v", end)
	}
	if notice := f.Concluded(t, stream, 1); notice.Event.Body != "Debate concluded: "+end.Reason+". The workstream stays in-shed until the owner decides.\n"+intake.Presentation("ratify: no objection stands") {
		t.Fatalf("notice %q", notice.Event.Body)
	}
	f.StillInShed(t, stream)
	if ran := f.Ran(); ran[intake.ReplyTurnID(1, 1)] != 0 || len(f.ReplyOperations(t, stream)) != 0 || len(f.RoundOperations(t, stream)) != 1 {
		t.Fatalf("consensus ran more than its round: %v", ran)
	}
	if open, err := intake.Dissent(f.Repository(), stream); err != nil || len(open) != 0 {
		t.Fatalf("dissent %+v %v", open, err)
	}

	fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).Pass(context.Background()))
	if moves := f.ShedMoves(t, stream); len(moves) != 3 {
		t.Fatalf("a pass after the conclusion moved the shed: %v", moves)
	}
}

func TestRedraftThenConcessionEndsTheDebate(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 3)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	size, fit := shed.ObjectionID(1, intake.CommitteeAgent(1), 1), shed.ObjectionID(1, intake.CommitteeAgent(2), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "kb/entities.json#internal.trace"))
	f.Member(1, 2, 1, intakefixture.Objects(p, shed.Fit, "plan", "spec#1"))
	var stream config.WorkstreamID
	f.Script(intake.ReplyTurnID(1, 1), map[string]string{plan.PlanPath: splitPlan}, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		intakefixture.CheckReplyBoundary(ctx, p, req, verified, tools, f.Clone)
		for _, want := range []string{"Round 1 of the shed is heard", "The committee debated spec.md revision 1 and plan.json revision 1. 2 objections stand",
			"- " + size + " (size, advisory, by " + intake.CommitteeAgent(1) + " in round 1 on plan#resume, citing kb/entities.json#internal.trace): It does not hold.",
			"- " + fit + " (fit, advisory, by " + intake.CommitteeAgent(2) + " in round 1 on plan, citing spec#1): It does not hold.", "one reply to this round", shed.ReplyTool, intake.DraftTool} {
			if !strings.Contains(req.Prompt, want) {
				p.Report("reply prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if strings.Contains(req.Prompt, "was not accepted") {
			p.Report("the first reply turn is told of a returned redraft:\n%s", req.Prompt)
		}
		if got, err := intakefixture.ReadTool(ctx, tools, shed.Path(1, intake.CommitteeAgent(1))); err != nil || !strings.Contains(got, size) {
			p.Report("the round's record: %q %v", got, err)
		}

		threads, err := f.Repository().Threads(stream)
		if err != nil {
			p.Report("threads: %v", err)
		}
		for _, th := range threads {
			for _, q := range th.Turns {
				if th.Identity.Role == intake.CommitteeRole && q.CompletedAt.IsZero() {
					p.Report("committee turn %s is in flight during the architect's reply", q.Request.TurnID)
				}
			}
		}
		if recorded, reason, err := intakefixture.ShedTool(ctx, tools, shed.ReplyTool, map[string]any{"objection": shed.ObjectionID(1, intake.CommitteeAgent(1), 2), "answer": "x"}); err != nil || recorded || !strings.Contains(reason, "stands after round 1") {
			p.Report("answered an objection nobody made: %v %q %v", recorded, reason, err)
		}
		return errors.Join(intakefixture.Answers(p, "Split in two.", size)(ctx, req, verified, tools), intakefixture.Answers(p, "The design resumes; so does the plan.", fit)(ctx, req, verified, tools))
	})
	f.Member(2, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, "Every member reads the same revision: spec.md revision 1 and plan.json revision 2") || !strings.Contains(req.Prompt, size) {
			p.Report("round 2 prompt:\n%s", req.Prompt)
		}
		if got, err := intakefixture.ReadTool(ctx, tools, "plan.json"); err != nil || got != splitPlan {
			p.Report("round 2 debates %q %v", got, err)
		}
		if got, err := intakefixture.ReadTool(ctx, tools, shed.ReplyPath(1)); err != nil || !strings.Contains(got, "Split in two.") {
			p.Report("the architect's reply in the view: %q %v", got, err)
		}
		return intakefixture.Concedes(p, size)(ctx, req, verified, tools)
	})
	f.Member(2, 2, 1, intakefixture.Concedes(p, fit))
	stream = f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-2")
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}

	ops := f.AcknowledgedReplies(t, stream)
	if len(ops) != 1 || !ops[0].Acknowledged || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" ||
		ops[0].Result.Evidence != "the architect answered 2 objections after round 1 and redrafted: spec.md revision 1 and plan.json revision 2" {
		t.Fatalf("reply operations: %+v", ops)
	}
	if in, err := intake.DecodeShed(ops[0].Operation, intake.ReplyAction); err != nil || in != (intake.RoundInput{Round: 1, Spec: 1, Plan: 1}) {
		t.Fatalf("reply input %+v %v", in, err)
	}
	asked, told := f.Transition(t, stream, "shed-reply-1"), f.Transition(t, stream, "shed-reply-1-replied")
	if asked.Cause != "shed-round-1-heard" || asked.Reason != "2 objections stand after round 1; the architect is asked for its reply" || told.Cause != ops[0].Operation.ID || told.Reason != ops[0].Result.Evidence {
		t.Fatalf("reply transitions: %+v %+v", asked, told)
	}
	reply := f.Reply(t, stream, 1)
	if reply.Round != 1 || reply.Revision != (shed.Pin{Spec: 1, Plan: 1}) || reply.Turn != intake.ReplyTurnID(1, 1) || reply.Redraft == nil || *reply.Redraft != (shed.Pin{Spec: 1, Plan: 2}) || reply.Failure != "" || len(reply.Problems) != 0 ||
		!slices.Equal(reply.Answers, []shed.Answer{{Objection: size, Answer: "Split in two."}, {Objection: fit, Answer: "The design resumes; so does the plan."}}) {
		t.Fatalf("reply: %+v", reply)
	}
	plans := f.Documents(t, stream, plan.PlanDocument)
	if len(plans) != 2 || plans[1].Content != splitPlan || plans[1].Actor != intake.ArchitectActor || plans[1].Cause != ops[0].Operation.ID || len(f.Documents(t, stream, plan.SpecDocument)) != 1 {
		t.Fatalf("plan revisions: %+v", plans)
	}
	landed := func(path string) string {
		return intakefixture.DocumentCommit(t, f.S.Current().Root, f.Project, stream, path)
	}
	if landed(plan.PlanPath) != landed(shed.ReplyPath(1)) {
		t.Fatal("the redraft and the reply landed in separate commits")
	}
	rounds := f.RoundOperations(t, stream)
	if len(rounds) != 2 {
		t.Fatalf("round operations: %+v", rounds)
	}
	for _, op := range rounds {
		if in, err := intake.DecodeRound(op.Operation); err != nil || in != (intake.RoundInput{Round: 1, Spec: 1, Plan: 1}) && in != (intake.RoundInput{Round: 2, Spec: 1, Plan: 2}) {
			t.Fatalf("round input %+v %v", in, err)
		}
	}
	if next := f.Transition(t, stream, "shed-round-2"); next.Cause != "shed-reply-1-replied" {
		t.Fatalf("round 2 request: %+v", next)
	}
	if end := f.Transition(t, stream, "shed-concluded-2"); end.Reason != "debate concluded by consensus after round 2: no objection stands" || end.Cause != "shed-round-2-heard" {
		t.Fatalf("conclusion: %+v", end)
	}
	f.Concluded(t, stream, 2)
	f.StillInShed(t, stream)
	if ran := f.Ran(); ran[intake.ReplyTurnID(1, 1)] != 1 || ran[intake.ReplyTurnID(1, 2)] != 0 || ran[intake.ReplyTurnID(2, 1)] != 0 {
		t.Fatalf("architect turns: %v", ran)
	}
	if open, err := intake.Dissent(f.Repository(), stream); err != nil || len(open) != 0 {
		t.Fatalf("dissent %+v %v", open, err)
	}
	root := f.Opts.Config.Root
	if _, err := os.Lstat(filepath.Join(root, "architect", string(f.Project), string(stream), intake.ReplyTurnID(1, 1), "workspace")); !os.IsNotExist(err) {
		t.Fatal("the reply's staged workspace is retained")
	}
	if entries, err := os.ReadDir(filepath.Join(root, "views")); err != nil || len(entries) != 0 {
		t.Fatalf("views leaked: %v %v", entries, err)
	}
}

func TestInvalidRedraftGoesBackToTheArchitect(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 2)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	proof := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Acceptance, "plan#dedupe", "spec#2"))
	f.Script(intake.ReplyTurnID(1, 1), map[string]string{plan.PlanPath: fixturekit.CyclicPlan}, intakefixture.Answers(p, "A test shows it now.", proof))
	f.Script(intake.ReplyTurnID(1, 2), map[string]string{plan.PlanPath: splitPlan}, func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, want := range []string{"Your redraft was not accepted, and the committee will not read it:", "- " + cycleProblem, "redraft/", "Your answers so far are kept", proof} {
			if !strings.Contains(req.Prompt, want) {
				p.Report("returned redraft prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if got, err := intakefixture.ReadTool(ctx, tools, "redraft/plan.json"); err != nil || got != fixturekit.CyclicPlan {
			p.Report("the returned redraft: %q %v", got, err)
		}
		if got, err := intakefixture.ReadTool(ctx, tools, "plan.json"); err != nil || got != fixturekit.ValidPlan {
			p.Report("the recorded plan: %q %v", got, err)
		}
		if _, err := intakefixture.ReadTool(ctx, tools, "redraft/spec.md"); err == nil {
			p.Report("a file the architect did not deliver is in redraft/")
		}
		return nil
	})
	f.Member(2, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if got, err := intakefixture.ReadTool(ctx, tools, "plan.json"); err != nil || got != splitPlan {
			p.Report("round 2 debates %q %v", got, err)
		}
		return intakefixture.Concedes(p, proof)(ctx, req, verified, tools)
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-2")
	p.Check(t)
	plans := f.Documents(t, stream, plan.PlanDocument)
	if len(plans) != 2 || plans[0].Content != fixturekit.ValidPlan || plans[1].Content != splitPlan {
		t.Fatalf("plan revisions: %+v", plans)
	}
	for path, content := range intakefixture.Snapshot(t, f.Trace) {
		if strings.Contains(content, `"depends_on": [\"dedupe\"]`) || strings.Contains(content, `"depends_on": ["dedupe"]`) {
			t.Fatalf("the invalid redraft reached the trace in %s", path)
		}
	}
	reply := f.Reply(t, stream, 1)
	if reply.Turn != intake.ReplyTurnID(1, 2) || reply.Redraft == nil || *reply.Redraft != (shed.Pin{Spec: 1, Plan: 2}) || len(reply.Problems) != 0 || !slices.Equal(reply.Answers, []shed.Answer{{Objection: proof, Answer: "A test shows it now."}}) {
		t.Fatalf("reply: %+v", reply)
	}
	if ops := f.SettledReplies(t, stream); len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("reply operations: %+v", ops)
	}
	if ran := f.Ran(); ran[intake.ReplyTurnID(1, 1)] != 1 || ran[intake.ReplyTurnID(1, 2)] != 1 || ran[intake.ReplyTurnID(1, 3)] != 0 {
		t.Fatalf("architect turns: %v", ran)
	}
	records, err := shed.Records(f.Repository(), stream)
	fixturekit.Must(t, err)
	if len(records) != 2 || records[1].Revision != (shed.Pin{Spec: 1, Plan: 2}) {
		t.Fatalf("records: %+v", records)
	}
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
}

func TestRedraftGivenUpLeavesTheRevision(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	for k := 1; k <= intake.MaxRedrafts; k++ {
		f.Script(intake.ReplyTurnID(1, k), map[string]string{plan.PlanPath: fixturekit.CyclicPlan}, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
			if returned := strings.Contains(req.Prompt, "Your redraft was not accepted"); returned != (k > 1) {
				p.Report("reply turn %d told of a returned redraft: %v", k, returned)
			}
			return nil
		})
	}
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	reply := f.Reply(t, stream, 1)
	if reply.Redraft != nil || !slices.Contains(reply.Problems, cycleProblem) || reply.Failure != "" || reply.Turn != intake.ReplyTurnID(1, intake.MaxRedrafts) {
		t.Fatalf("reply: %+v", reply)
	}
	if plans := f.Documents(t, stream, plan.PlanDocument); len(plans) != 1 {
		t.Fatalf("plan revisions: %+v", plans)
	}
	told := f.Transition(t, stream, "shed-reply-1-replied")
	if !strings.HasPrefix(told.Reason, "the architect answered 0 objections after round 1; its redraft was given up and spec.md revision 1 and plan.json revision 1 stays:\n- ") || !strings.Contains(told.Reason, cycleProblem) {
		t.Fatalf("replied reason %q", told.Reason)
	}
	if ran := f.Ran(); ran[intake.ReplyTurnID(1, intake.MaxRedrafts)] != 1 || ran[intake.ReplyTurnID(1, intake.MaxRedrafts+1)] != 0 {
		t.Fatalf("architect turns: %v", ran)
	}
	if open, err := intake.Dissent(f.Repository(), stream); err != nil || len(open) != 1 || open[0].ID != size {
		t.Fatalf("dissent %+v %v", open, err)
	}
}

func TestCapReachedWithAnOpenVetoApprovesNothing(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 2)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	veto, advice := shed.ObjectionID(1, intake.CommitteeAgent(1), 1), shed.ObjectionID(1, intake.CommitteeAgent(2), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Charter, "spec#2", "charter#2"))
	f.Member(1, 2, 1, intakefixture.Objects(p, shed.Fit, "plan", "spec#1"))

	f.Script(intake.ReplyTurnID(1, 1), map[string]string{plan.PlanPath: fixturekit.ValidPlan}, intakefixture.Answers(p, "Criterion 2 has a test.", veto))

	f.Member(2, 1, 1, intakefixture.Silent)
	f.Member(2, 2, 1, intakefixture.Silent)
	f.Script(intake.ReplyTurnID(2, 1), nil, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, "Round 2 of the shed is heard") || !strings.Contains(req.Prompt, veto+" (charter, blocking") {
			p.Report("round 2 reply prompt:\n%s", req.Prompt)
		}
		return nil
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-2")
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "reply-2", "replied-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	end := f.Transition(t, stream, "shed-concluded-2")
	if end.Reason != "debate stopped after round 2, at the shed.max_rounds cap of 2, with 2 objections standing, 1 of them blocking; the cap approves nothing" || end.Cause != "shed-reply-2-replied" || end.From != "replied-2" {
		t.Fatalf("conclusion: %+v", end)
	}
	notice := f.Concluded(t, stream, 2).Event.Body
	for _, want := range []string{"Debate concluded: " + end.Reason + ". The workstream stays in-shed until the owner decides.", "Open dissent:", intake.Presentation(shed.Recommend([]shed.Entry{{Blocking: true, Dissent: shed.Dissent{Objection: shed.Objection{ID: veto}}}})),
		"- " + veto + " (charter, blocking, by " + intake.CommitteeAgent(1) + " in round 1 on spec#2, against spec.md revision 1 and plan.json revision 1): It does not hold.",
		"- " + advice + " (fit, advisory, by " + intake.CommitteeAgent(2) + " in round 1 on plan, against spec.md revision 1 and plan.json revision 1): It does not hold."} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice lacks %q:\n%s", want, notice)
		}
	}
	f.StillInShed(t, stream)
	if reply := f.Reply(t, stream, 1); reply.Redraft != nil || len(f.Documents(t, stream, plan.PlanDocument)) != 1 {
		t.Fatalf("an unchanged plan was recorded as a redraft: %+v", reply)
	}
	if delivered := f.Delivered(intake.ReplyTurnID(1, 1)); len(delivered) != 1 || delivered[0].Verdict != intake.DraftUnchanged || len(delivered[0].Problems) != 0 {
		t.Fatalf("an unchanged plan was delivered as %+v", delivered)
	}
	open, err := intake.Dissent(f.Repository(), stream)
	fixturekit.Must(t, err)
	if len(open) != 2 || open[0].ID != veto || open[0].Kind != shed.Charter || open[0].Member != intake.CommitteeAgent(1) || open[0].Part != "spec#2" || !open[0].Blocking ||
		open[1].ID != advice || open[1].Kind != shed.Fit || open[1].Member != intake.CommitteeAgent(2) || open[1].Part != "plan" || open[1].Blocking {
		t.Fatalf("dissent record: %+v", open)
	}

	fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).Pass(context.Background()))
	if ran := f.Ran(); ran[intake.RoundTurnID(3, intake.CommitteeAgent(1), 1)] != 0 || len(f.RoundOperations(t, stream)) != 2 || len(f.ReplyOperations(t, stream)) != 2 || ran[intake.ReplyTurnID(1, 1)] != 1 || ran[intake.ReplyTurnID(2, 1)] != 1 {
		t.Fatalf("turns after the cap: %v", ran)
	}
	if moves := f.ShedMoves(t, stream); len(moves) != 9 {
		t.Fatalf("a pass after the conclusion moved the shed: %v", moves)
	}
}

func TestFailedReplyIsRecordedAndTheDebateGoesOn(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), map[string]string{plan.PlanPath: splitPlan}, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(intakefixture.Answers(p, "Half an answer.", size)(ctx, req, verified, tools), errors.New("the agent crashed"))
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	reply := f.Reply(t, stream, 1)
	if !strings.Contains(reply.Failure, "the agent crashed") || reply.Redraft != nil || len(reply.Answers) != 1 || reply.Turn != intake.ReplyTurnID(1, 1) {
		t.Fatalf("reply: %+v", reply)
	}
	if told := f.Transition(t, stream, "shed-reply-1-replied"); !strings.Contains(told.Reason, "its turn failed: ") || !strings.Contains(told.Reason, "the agent crashed") {
		t.Fatalf("replied reason %q", told.Reason)
	}
	if ran := f.Ran(); ran[intake.ReplyTurnID(1, 1)] != 1 || ran[intake.ReplyTurnID(1, 2)] != 0 || len(f.Documents(t, stream, plan.PlanDocument)) != 1 {
		t.Fatalf("a failed turn was retried or its redraft recorded: %v", ran)
	}
}

func TestDebateResumesMidRoundAfterARestart(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 2)
	p := &intakefixture.Faults{}
	size, fit := shed.ObjectionID(1, intake.CommitteeAgent(1), 1), shed.ObjectionID(1, intake.CommitteeAgent(2), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Charter, "plan#resume", "charter#1"))
	f.Member(1, 2, 1, intakefixture.Objects(p, shed.Fit, "plan", "spec#1"))
	replying := make(chan struct{})
	f.Script(intake.ReplyTurnID(1, 1), nil, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {

		intakefixture.Answers(p, "Lost.", fit)(ctx, req, verified, tools)
		close(replying)
		<-ctx.Done()
		return ctx.Err()
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	wait := func(entered chan struct{}, what string) {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal(what + " did not start")
		}
	}
	wait(replying, "the architect's reply")
	f.Stop(t)

	f.Script(intake.ReplyTurnID(1, 2), nil, intakefixture.Answers(p, "Kept.", size))
	debating := make(chan struct{})
	f.Member(2, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(debating)
		<-ctx.Done()
		return ctx.Err()
	})
	f.Member(2, 2, 1, intakefixture.Concedes(p, fit))
	f.Start(t)
	wait(debating, "round 2")
	f.S.Until(t, "member 2 to finish round 2", func() bool {
		th, err := f.Repository().Thread(stream, intake.CommitteeAgent(2))
		fixturekit.Must(t, err)
		return len(th.Turns) == 2 && th.Turns[1].Status() == "idle"
	})
	f.Stop(t)

	f.Member(2, 1, 2, intakefixture.Concedes(p, size))
	f.Start(t)
	defer f.Stop(t)
	f.AwaitShed(t, stream, "concluded-2")
	p.Check(t)
	want := map[string]int{"draft-1-1": 1, intake.RoundTurnID(1, intake.CommitteeAgent(1), 1): 1, intake.RoundTurnID(1, intake.CommitteeAgent(2), 1): 1, intake.ReplyTurnID(1, 1): 1, intake.ReplyTurnID(1, 2): 1,
		intake.RoundTurnID(2, intake.CommitteeAgent(1), 1): 1, intake.RoundTurnID(2, intake.CommitteeAgent(2), 1): 1, intake.RoundTurnID(2, intake.CommitteeAgent(1), 2): 1}
	for turn, n := range f.Ran() {
		if want[turn] != n {
			t.Fatalf("turn %s ran %d times, want %d: %v", turn, n, want[turn], f.Ran())
		}
	}
	if reply := f.Reply(t, stream, 1); reply.Turn != intake.ReplyTurnID(1, 2) || !slices.Equal(reply.Answers, []shed.Answer{{Objection: size, Answer: "Kept."}}) {
		t.Fatalf("reply: %+v", reply)
	}
	for round := 1; round <= 2; round++ {
		for i := 1; i <= 2; i++ {
			if docs := f.Documents(t, stream, shed.DocumentID(round, intake.CommitteeAgent(i))); len(docs) != 1 {
				t.Fatalf("round %d records of member %d: %+v", round, i, docs)
			}
		}
	}
	if len(f.ReplyOperations(t, stream)) != 1 || len(f.RoundOperations(t, stream)) != 2 {
		t.Fatalf("operations: %+v %+v", f.ReplyOperations(t, stream), f.RoundOperations(t, stream))
	}
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "replied-1", "round-2", "heard-2", "concluded-2"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
}

func TestReplyRecordedBeforeAStopIsNotRecordedAgain(t *testing.T) {
	t.Parallel()
	for _, crash := range []string{"recorded-then-abandoned", "answered-then-abandoned"} {
		t.Run(crash, func(t *testing.T) {
			f := journey.NewDebateFixture(t, 1, 1)
			ctx := context.Background()
			p := &intakefixture.Faults{}
			size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
			f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
			entered := make(chan struct{})
			f.Script(intake.ReplyTurnID(1, 1), nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			})
			stream := f.HandIn(t, "design", fixturekit.HandedDesign)
			select {
			case <-entered:
			case <-time.After(fixturekit.DemoTimeout):
				t.Fatal("the reply did not start")
			}
			f.Stop(t)
			p.Check(t)

			cfg, err := projectstore.LoadConfig(f.Opts.Config)
			fixturekit.Must(t, err)
			repo, err := trace.Open(cfg.Root, config.Project{ID: f.Project, Clone: f.Clone})
			fixturekit.Must(t, err)
			ops, err := repo.Operations(stream)
			fixturekit.Must(t, err)
			operation := ""
			for _, o := range ops {
				if o.Operation.Action == intake.ReplyAction {
					operation = o.Operation.ID
				}
			}
			th, err := repo.Thread(stream, intake.ArchitectAgent)
			fixturekit.Must(t, err)
			first := th.Turns[len(th.Turns)-1]
			if first.Request.TurnID != intake.ReplyTurnID(1, 1) || first.Status() != "interrupted" || first.Request.Cause != operation {
				t.Fatalf("interrupted turn: %+v", first)
			}
			second := first.Request
			second.TurnID = intake.ReplyTurnID(1, 2)
			second.ID, second.At = "request_"+second.TurnID, f.Clock.Now()
			_, err = repo.EnqueueTurn(ctx, second)
			fixturekit.Must(t, err)
			directory := filepath.Join(cfg.Root.String(), "architect", string(f.Project), string(stream), second.TurnID)
			claimed, err := repo.ClaimTurn(ctx, stream, intake.ArchitectAgent, "earlier-session", filepath.Join(directory, "session"), f.Clock.Now())
			fixturekit.Must(t, err)
			h := second.Header
			h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", trace.EventID(second.ID, "response"), f.Clock.Now(), trace.Actor{Kind: "service", ID: "thread-runner"}
			response := trace.TurnResponse{Header: h, AgentID: intake.ArchitectAgent, ThreadID: second.ThreadID, TurnID: second.TurnID, RequestID: second.ID, RequestRevision: second.Revision,
				Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: second.Profile.Backend, ID: "session-2"}, SessionDirectory: claimed.Claim.SessionDirectory, StartedAt: claimed.Claim.At, Duration: time.Second, FinalResponse: "Replied"}}
			fixturekit.Must(t, repo.CaptureTurn(ctx, "earlier-session", response))
			fixturekit.Must(t, repo.CompleteTurn(ctx, stream, intake.ArchitectAgent, second.TurnID, "earlier-session", f.Clock.Now()))
			data, err := shed.EncodeReply(shed.Reply{Version: shed.Version, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, Turn: second.TurnID, Answers: []shed.Answer{{Objection: size, Answer: "Recorded."}}})
			fixturekit.Must(t, err)
			if crash == "answered-then-abandoned" {

				fixturekit.Must(t, os.MkdirAll(filepath.Join(directory, "output"), 0700))
				fixturekit.Must(t, os.WriteFile(filepath.Join(directory, "output", intake.ReplyFile), data, 0600))
				fixturekit.Must(t, os.WriteFile(filepath.Join(directory, "output", plan.PlanPath), []byte(splitPlan), 0600))
			} else {
				fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: shed.ReplyDocumentID(1), Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.ArchitectActor, Cause: operation, Depth: 1},
					Path: shed.ReplyPath(1), Content: string(data)}}))
			}
			abandoned := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "abandoned", Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: intake.OwnerActor, Cause: "owner"}
			_, err = repo.SetFeatureState(ctx, abandoned, intake.AbandonedState, "the owner abandoned the workstream")
			fixturekit.Must(t, err)
			fixturekit.Must(t, repo.Close())

			f.Start(t)
			defer f.Stop(t)
			if crash == "answered-then-abandoned" {
				f.AwaitShed(t, stream, "failed-1", "replied-1")
				replies := f.SettledReplies(t, stream)
				if len(replies) != 1 || replies[0].Result.Outcome != "failed" || replies[0].Result.Evidence != "the reply to round 1 failed: the workstream was abandoned, so the architect's reply is not recorded" {
					t.Fatalf("operations: %+v", replies)
				}
				if docs := f.Documents(t, stream, shed.ReplyDocumentID(1)); len(docs) != 0 || len(f.Documents(t, stream, plan.PlanDocument)) != 1 {
					t.Fatalf("an abandoned workstream's reply was recorded: %+v", docs)
				}
				if ran := f.Ran(); ran[intake.ReplyTurnID(1, 2)] != 0 || ran[intake.ReplyTurnID(1, 3)] != 0 {
					t.Fatalf("architect turns: %v", ran)
				}
				return
			}
			f.AwaitShed(t, stream, "replied-1")
			if ran := f.Ran(); ran[intake.ReplyTurnID(1, 1)] != 1 || ran[intake.ReplyTurnID(1, 2)] != 0 || ran[intake.ReplyTurnID(1, 3)] != 0 {
				t.Fatalf("architect turns: %v", ran)
			}
			if docs := f.Documents(t, stream, shed.ReplyDocumentID(1)); len(docs) != 1 || docs[0].Content != string(data) {
				t.Fatalf("reply documents: %+v", docs)
			}
			replies := f.SettledReplies(t, stream)
			if len(replies) != 1 || replies[0].Result == nil || replies[0].Result.Outcome != "succeeded" || replies[0].Result.Evidence != "the architect answered 1 objections after round 1 and left spec.md revision 1 and plan.json revision 1 as it is" {
				t.Fatalf("operations: %+v", replies)
			}

			fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).Pass(ctx))
			if moves := f.ShedMoves(t, stream); moves[len(moves)-1] != "replied-1" {
				t.Fatalf("shed of an abandoned workstream went %v", moves)
			}
		})
	}
}

func TestAbandonFailsARunningReply(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 2)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	entered := make(chan struct{})
	f.Script(intake.ReplyTurnID(1, 1), nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the reply never started")
	}
	_, err := f.C.Abandon(context.Background(), stream, "no longer needed")
	fixturekit.Must(t, err)
	f.AwaitShed(t, stream, "failed-1", "replied-1")
	p.Check(t)
	if replies, err := shed.Replies(f.Repository(), stream); err != nil || len(replies) != 0 {
		t.Fatalf("replies of an abandoned workstream: %+v %v", replies, err)
	}
	ops := f.SettledReplies(t, stream)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "failed" || ops[0].Result.Evidence != "the reply to round 1 failed: the workstream was abandoned, so the architect's reply is not recorded" {
		t.Fatalf("operations: %+v", ops)
	}
	if failed := f.Transition(t, stream, "shed-reply-1-failed"); failed.From != "reply-1" || failed.Reason != ops[0].Result.Evidence {
		t.Fatalf("failed transition: %+v", failed)
	}
	if ran := f.Ran(); ran[intake.ReplyTurnID(1, 2)] != 0 || ran[intake.RoundTurnID(2, intake.CommitteeAgent(1), 1)] != 0 {
		t.Fatalf("turns after abandonment: %v", ran)
	}
}

func TestReplyWaitsForAnArchitectRunner(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	runner := f.Opts.Architect
	size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	entered := make(chan struct{})
	f.Member(1, 1, 1, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	select {
	case <-entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the round did not start")
	}
	f.Stop(t)

	f.Member(1, 1, 2, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	f.Opts.Architect = nil
	f.Start(t)
	f.AwaitShed(t, stream, "heard-1")
	d := &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}
	fixturekit.Must(t, d.Pass(ctx))
	if moves := f.ShedMoves(t, stream); moves[len(moves)-1] != "heard-1" || len(f.ReplyOperations(t, stream)) != 0 {
		t.Fatalf("a reply was requested without a runner: %v", moves)
	}
	f.Stop(t)

	replying := make(chan struct{})
	f.Script(intake.ReplyTurnID(1, 1), nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(replying)
		<-ctx.Done()
		return ctx.Err()
	})
	f.Opts.Architect = runner
	f.Start(t)
	select {
	case <-replying:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the reply did not start")
	}
	f.Stop(t)

	f.Opts.Architect = nil
	f.Start(t)
	ops := f.ReplyOperations(t, stream)
	if len(ops) != 1 {
		t.Fatalf("operations: %+v", ops)
	}
	r := intake.Replier{Debate: &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}}
	if observed, err := r.Inspect(ctx, ops[0].Operation); err != nil || observed.State != coreadapter.EffectAbsent {
		t.Fatalf("inspect an interrupted reply: %+v %v", observed, err)
	}
	if _, err := r.Apply(ctx, ops[0].Operation); !errors.Is(err, intake.ErrNoArchitect) {
		t.Fatalf("apply without a runner: %v", err)
	}
	th, err := f.Repository().Thread(stream, intake.ArchitectAgent)
	fixturekit.Must(t, err)
	if turns := intake.ChainTurns(th, (intake.RoundInput{Round: 1}).Chain(th, nil)); len(turns) != 1 || turns[0].Status() != "interrupted" {
		t.Fatalf("a turn was queued without a runner: %+v", turns)
	}
	f.Stop(t)

	f.Script(intake.ReplyTurnID(1, 2), nil, intakefixture.Answers(p, "Split.", size))
	f.Opts.Architect = runner
	f.Start(t)
	defer f.Stop(t)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	if reply := f.Reply(t, stream, 1); reply.Turn != intake.ReplyTurnID(1, 2) || len(reply.Answers) != 1 {
		t.Fatalf("reply: %+v", reply)
	}
}

func TestReplyInterruptedEveryAttemptIsRecordedAsFailed(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	for k := 1; k <= intake.MaxReplyAttempts; k++ {
		f.Script(intake.ReplyTurnID(1, k), nil, func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error {
			return errors.Join(context.Canceled, errors.New("connection dropped"))
		})
	}
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	if reply, want := f.Reply(t, stream, 1), fmt.Sprintf("the architect's turn was interrupted %d times by service stops", intake.MaxReplyAttempts); reply.Failure != want || reply.Turn != "" || len(reply.Answers) != 0 {
		t.Fatalf("reply: %+v", reply)
	}
	if ran := f.Ran(); ran[intake.ReplyTurnID(1, intake.MaxReplyAttempts)] != 1 || ran[intake.ReplyTurnID(1, intake.MaxReplyAttempts+1)] != 0 {
		t.Fatalf("architect turns: %v", ran)
	}
}

func TestReplyToolsAreBoundToTheReplyTurn(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	ctx := context.Background()
	path := (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).ReplyPath(stream, intake.RoundInput{Round: 1, Spec: 1, Plan: 1})
	valid := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Role: intake.ArchitectRole, Thread: intake.ArchitectThread, Turn: intake.ReplyTurnID(1, 2)}
	tools, err := path.Scoped(ctx, valid)
	fixturekit.Must(t, err)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	if want := []string{shed.ReplyTool, intake.DraftTool, questions.AskTool, service.ProjectContextTool}; !slices.Equal(names, want) {
		t.Fatalf("tools %v, want %v", names, want)
	}
	for name, mutate := range map[string]func(*coreadapter.Scope){
		"another project":    func(s *coreadapter.Scope) { s.Project = "other" },
		"another workstream": func(s *coreadapter.Scope) { s.Workstream = "w_fedcba9876543210fedcba9876543210" },
		"another role":       func(s *coreadapter.Scope) { s.Role = intake.CommitteeRole },
		"another thread":     func(s *coreadapter.Scope) { s.Thread = intake.CommitteeThread(1) },
		"a drafting turn":    func(s *coreadapter.Scope) { s.Turn = intake.DraftTurnID(1, 1) },
		"another round":      func(s *coreadapter.Scope) { s.Turn = intake.ReplyTurnID(2, 1) },
	} {
		scope := valid
		mutate(&scope)
		if tools, err := path.Scoped(ctx, scope); err == nil {
			t.Errorf("%s: got %d tools", name, len(tools))
		}
	}
}

func TestReturnedTurnThatDeliversNothingLeavesTheRevision(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), map[string]string{plan.PlanPath: fixturekit.CyclicPlan}, intakefixture.Answers(p, "Split.", size))
	f.Script(intake.ReplyTurnID(1, 2), nil, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, "Deliver nothing to leave the revision as it is") {
			p.Report("returned redraft prompt:\n%s", req.Prompt)
		}
		return nil
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	reply := f.Reply(t, stream, 1)
	if reply.Redraft != nil || len(reply.Problems) != 0 || reply.Failure != "" || reply.Turn != intake.ReplyTurnID(1, 2) || len(reply.Answers) != 1 {
		t.Fatalf("reply: %+v", reply)
	}
	if plans := f.Documents(t, stream, plan.PlanDocument); len(plans) != 1 || plans[0].Content != fixturekit.ValidPlan {
		t.Fatalf("plan revisions: %+v", plans)
	}
	if told := f.Transition(t, stream, "shed-reply-1-replied"); told.Reason != "the architect answered 1 objections after round 1 and left spec.md revision 1 and plan.json revision 1 as it is" {
		t.Fatalf("replied reason %q", told.Reason)
	}
	if ran := f.Ran(); ran[intake.ReplyTurnID(1, 2)] != 1 || ran[intake.ReplyTurnID(1, 3)] != 0 {
		t.Fatalf("architect turns: %v", ran)
	}
}

func TestSpecRedraftLeavesThePlanAtItsRevision(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 2)
	defer f.Stop(t)
	p := &intakefixture.Faults{}
	fit := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	revised := strings.Replace(fixturekit.ValidSpec, "It must not re-send acknowledged chunks.", "It never sends an acknowledged chunk again.", 1)
	if revised == fixturekit.ValidSpec {
		t.Fatal("the revised spec equals the draft")
	}
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Fit, "spec", "charter#1"))
	f.Script(intake.ReplyTurnID(1, 1), map[string]string{plan.SpecPath: revised}, intakefixture.Answers(p, "Reworded.", fit))
	f.Member(2, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, "spec.md revision 2 and plan.json revision 1") {
			p.Report("round 2 prompt:\n%s", req.Prompt)
		}
		if got, err := intakefixture.ReadTool(ctx, tools, "spec.md"); err != nil || got != revised {
			p.Report("round 2 debates %q %v", got, err)
		}
		return intakefixture.Concedes(p, fit)(ctx, req, verified, tools)
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-2")
	p.Check(t)
	specs := f.Documents(t, stream, plan.SpecDocument)
	if len(specs) != 2 || specs[1].Content != revised || specs[1].Actor != intake.ArchitectActor || len(f.Documents(t, stream, plan.PlanDocument)) != 1 {
		t.Fatalf("spec revisions: %+v", specs)
	}
	if reply := f.Reply(t, stream, 1); reply.Redraft == nil || *reply.Redraft != (shed.Pin{Spec: 2, Plan: 1}) {
		t.Fatalf("reply: %+v", reply)
	}
	if told := f.Transition(t, stream, "shed-reply-1-replied"); told.Reason != "the architect answered 1 objections after round 1 and redrafted: spec.md revision 2 and plan.json revision 1" {
		t.Fatalf("replied reason %q", told.Reason)
	}
}

func TestRoundWhereEveryTurnFailedConcludesWithoutAReview(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 2, 3)
	defer f.Stop(t)
	crash := func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error {
		return errors.New("the agent crashed")
	}
	f.Member(1, 1, 1, crash)
	f.Member(1, 2, 1, crash)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	end := f.Transition(t, stream, "shed-concluded-1")
	if want := "debate concluded after round 1 without a review: the turns of all 2 members failed, so no objection stands and nobody agreed"; end.Reason != want {
		t.Fatalf("conclusion %q, want %q", end.Reason, want)
	}
	if notice := f.Concluded(t, stream, 1).Event.Body; notice != "Debate concluded: "+end.Reason+". The workstream stays in-shed until the owner decides.\n"+intake.Presentation("ratify: no objection stands") || strings.Contains(notice, "consensus") {
		t.Fatalf("notice %q", notice)
	}
	f.StillInShed(t, stream)
	if ran := f.Ran(); ran[intake.ReplyTurnID(1, 1)] != 0 {
		t.Fatalf("the architect replied to nothing: %v", ran)
	}
}

func TestUnopposedConclusionNamesFailedTurns(t *testing.T) {
	t.Parallel()
	heard := shed.Record{Round: 2, Member: intake.CommitteeAgent(1)}
	failed := shed.Record{Round: 2, Member: intake.CommitteeAgent(2), Failure: "the agent crashed"}
	alsoFailed := shed.Record{Round: 2, Member: intake.CommitteeAgent(1), Failure: "the agent crashed"}
	earlierHeard := shed.Record{Round: 1, Member: intake.CommitteeAgent(2)}
	earlierFailed := shed.Record{Round: 1, Member: intake.CommitteeAgent(1), Failure: "the agent crashed"}
	owner := shed.Record{Round: 2, Member: shed.OwnerMember}
	for name, tc := range map[string]struct {
		records []shed.Record
		open    []shed.Entry
		want    string
	}{
		"consensus":          {[]shed.Record{earlierFailed, heard, owner}, nil, "debate concluded by consensus after round 2: no objection stands"},
		"some turns failed":  {[]shed.Record{heard, failed, owner}, nil, "debate concluded by consensus after round 2: no objection stands; the turns of 1 of 2 members failed"},
		"every turn failed":  {[]shed.Record{earlierHeard, alsoFailed, failed, owner}, nil, "debate concluded after round 2 without a review: the turns of all 2 members failed, so no objection stands and nobody agreed"},
		"the owner disposed": {[]shed.Record{heard, failed}, []shed.Entry{{Disposition: shed.Dismissed}}, "debate concluded after round 2: the owner disposed of every objection that stood"},
	} {
		if got := intake.Unopposed(tc.records, tc.open, 2); got != tc.want {
			t.Fatalf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestDebateWithoutACommitteeRunnerStillRepliesAndConcludes(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 2)
	ctx := context.Background()
	p := &intakefixture.Faults{}
	size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Charter, "plan#resume", "charter#1"))
	replying := make(chan struct{})
	f.Script(intake.ReplyTurnID(1, 1), nil, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(replying)
		<-ctx.Done()
		return ctx.Err()
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	select {
	case <-replying:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the reply did not start")
	}
	f.Stop(t)

	f.Script(intake.ReplyTurnID(1, 2), nil, intakefixture.Answers(p, "Split.", size))
	f.Opts.Committee = nil
	f.Start(t)
	f.AwaitShed(t, stream, "replied-1")
	f.SettledReplies(t, stream)
	fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).Pass(ctx))
	if moves := f.ShedMoves(t, stream); moves[len(moves)-1] != "replied-1" || len(f.RoundOperations(t, stream)) != 1 {
		t.Fatalf("a round was requested without a committee runner: %v", moves)
	}
	f.Stop(t)
	p.Check(t)

	f.SetRounds(t, 2, 1)
	f.Start(t)
	defer f.Stop(t)
	f.AwaitShed(t, stream, "concluded-1")
	if end, want := f.Transition(t, stream, "shed-concluded-1"), "debate stopped after round 1, at the shed.max_rounds cap of 1, with 1 objection standing, 1 of them blocking; the cap approves nothing"; end.Reason != want {
		t.Fatalf("conclusion %q, want %q", end.Reason, want)
	}
	if ran := f.Ran(); ran[intake.RoundTurnID(2, intake.CommitteeAgent(1), 1)] != 0 {
		t.Fatalf("round 2 ran: %v", ran)
	}
}

func TestFirstRoundWaitsForACommitteeRunner(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixture(t, 1, 1)
	ctx := context.Background()
	runner := f.Opts.Committee
	f.Stop(t)
	f.Opts.Committee = nil
	f.Start(t)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.Sketched)

	d := &intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}
	fixturekit.Must(t, d.EnsureCommittee(ctx, stream, 1))
	_, err := f.Repository().MoveFeatureState(ctx, d.Header(intake.InShedState, stream, intake.SketchedState, f.Clock.Now()), intake.SketchedState, intake.InShedState, "planted")
	fixturekit.Must(t, err)
	fixturekit.Must(t, d.Pass(ctx))
	if moves := f.ShedMoves(t, stream); len(moves) != 0 || len(f.RoundOperations(t, stream)) != 0 {
		t.Fatalf("a round was requested without a committee runner: %v", moves)
	}
	f.Stop(t)

	f.Member(1, 1, 1, intakefixture.Silent)
	f.Opts.Committee = runner
	f.Start(t)
	defer f.Stop(t)
	f.AwaitShed(t, stream, "concluded-1")
}
