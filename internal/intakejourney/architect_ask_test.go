package intakejourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	slices "slices"
	strings "strings"
	testing "testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	agent "github.com/kpenfound/agentcore/agent"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestArchitectQuestionParksTheDraftUntilTheAnswerArrives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	f, _ := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	asking := intake.DraftTurnID(1, 1)
	f.Script(asking, nil, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, "Call ask when something you must know to draft is not in your view") || !strings.Contains(req.SystemPrompt, "call ask: your work waits for the answer") {
			p.Report("draft prompts do not offer ask:\n%s\n%s", req.SystemPrompt, req.Prompt)
		}
		return errors.Join(intakefixture.Delivers(p, plan.SpecPath, fixturekit.ValidSpec, intake.DraftInvalid)(ctx, req, verified, tools), intakefixture.Asks(p, "1")(ctx, req, verified, tools))
	})
	answering := f.Answer("1", func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		for _, want := range []string{"The owner ruled on your question 1. The manager relays the ruling.", "| " + intakefixture.AskedQuestion, "<<< osmia:owner_response", "| " + intakefixture.RelayedRuling} {
			if !strings.Contains(req.Prompt, want) {
				p.Report("answer prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if !strings.Contains(req.SystemPrompt, "architect of the dagger project") || req.Profile.Name != intake.ArchitectRole {
			p.Report("answer turn system prompt %q, profile %+v", req.SystemPrompt, req.Profile)
		}
		if names, err := intakefixture.ToolNames(ctx, tools); err != nil || !slices.Equal(names, []string{questions.AskTool, intake.DraftTool, "factory_context", "file_read", "inspect_code", "project_context", "read_remote_file"}) {
			p.Report("answer turn tools %v %v", names, err)
		}
		return intakefixture.Delivers(p, plan.PlanPath, fixturekit.ValidPlan, intake.DraftValid)(ctx, req, verified, tools)
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.Await(t, stream, intakefixture.DraftAt("waiting-1"))
	p.Check(t)

	ops := f.AcknowledgedDraftOperations(t, stream)
	parked := "draft 1 is parked: the architect waits for the answer to question 1"
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != questions.Waiting || ops[0].Result.Evidence != parked {
		t.Fatalf("draft operations: %v", intakefixture.Results(ops))
	}
	park := f.Transition(t, stream, "draft-1-waiting-1")
	if park.From != "drafting-1" || park.To != "waiting-1" || park.Cause != ops[0].Operation.ID || park.Reason != parked || park.Actor != intake.DraftingActor {
		t.Fatalf("park: %+v", park)
	}

	th := f.ArchitectThread(t, stream)
	if last := th.Turns[len(th.Turns)-1]; !th.Parked() || last.Request.TurnID != asking || last.Status() != "waiting" || last.Response.Result.Outcome == nil || *last.Response.Result.Outcome != (coreadapter.Outcome{Status: "waiting", Report: "Asked question 1"}) {
		t.Fatalf("architect's thread: %+v", th)
	}
	if q := f.Question(t, stream, "1"); q.Asked.AskedBy.ID != intake.ArchitectAgent || q.Asked.Thread != intake.ArchitectThread || q.Asked.Turn != asking || q.Asked.Question != intakefixture.AskedQuestion {
		t.Fatalf("question: %+v", q)
	}
	if specs, plans := f.Documents(t, stream, plan.SpecDocument), f.Documents(t, stream, plan.PlanDocument); len(specs) != 0 || len(plans) != 0 {
		t.Fatalf("a parked draft recorded %+v %+v", specs, plans)
	}
	if state, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != intake.HandedState {
		t.Fatalf("feature %+v %v", state, err)
	}

	f.AwaitQuestion(t, stream, "1", trace.QuestionEscalated)
	f.Stop(t)
	f.Start(t)
	if state, err := f.Repository().Workflow(stream, intake.DraftSubject); err != nil || state.Value != "waiting-1" {
		t.Fatalf("draft after the restart: %+v %v", state, err)
	}
	fixturekit.Must(t, (&intake.Drafter{S: f.S.Intake().DrafterDependencies(), Repository: f.Repository()}).Pass(ctx))
	if moves, want := f.DraftMoves(t, stream), []string{"draft-1  -> drafting-1", "draft-1-waiting-1 drafting-1 -> waiting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("draft went %v after the restart, want %v", moves, want)
	}

	f.Rule(t, "1")
	f.Await(t, stream, f.TransitionRecorded(t, stream, intake.SketchedState))
	p.Check(t)
	if moves, want := f.DraftMoves(t, stream), []string{"draft-1  -> drafting-1", "draft-1-waiting-1 drafting-1 -> waiting-1", "draft-1-resume-1 waiting-1 -> drafting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("draft went %v, want %v", moves, want)
	}
	resumption := f.Transition(t, stream, "draft-1-resume-1")
	if resumption.Cause != "draft-1-waiting-1" || resumption.Actor != intake.DraftingActor || resumption.Reason != "draft 1 resumes: the architect's question is answered" {
		t.Fatalf("resumption: %+v", resumption)
	}
	ops = f.AcknowledgedDraftOperations(t, stream)
	if len(ops) != 2 || ops[1].Result == nil || ops[1].Result.Outcome != "succeeded" {
		t.Fatalf("draft operations: %v", intakefixture.Results(ops))
	}
	var in intake.DraftInput
	if err := json.Unmarshal(ops[1].Operation.Input, &in); err != nil || in != (intake.DraftInput{Draft: 1, Resume: 1}) {
		t.Fatalf("resumption input %+v %v", in, err)
	}
	if ops[1].Operation.ID != trace.OperationID(f.Project, stream, trace.EventID("draft-1-resume-1", "run")) {
		t.Fatalf("resumption operation %s", ops[1].Operation.ID)
	}
	if told := f.Transition(t, stream, intake.SketchedState); told.Cause != ops[1].Operation.ID || !strings.HasPrefix(told.Reason, "the architect's draft 1 passed validation: spec.md revision 1") {
		t.Fatalf("sketched: %+v", told)
	}
	specs, plans := f.Documents(t, stream, plan.SpecDocument), f.Documents(t, stream, plan.PlanDocument)
	if len(specs) != 1 || specs[0].Content != fixturekit.ValidSpec || specs[0].Cause != ops[1].Operation.ID || len(plans) != 1 || plans[0].Content != fixturekit.ValidPlan {
		t.Fatalf("draft documents %+v %+v", specs, plans)
	}
	if ran := f.Ran(); ran[asking] != 1 || ran[answering] != 1 || ran[intake.DraftTurnID(1, 2)] != 0 || ran[intake.DraftTurnID(2, 1)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	th = f.ArchitectThread(t, stream)
	if th.Parked() || len(th.Turns) < 2 || th.Turns[1].Request.TurnID != answering || th.Turns[1].Status() != "idle" || th.Turns[1].Request.SystemPrompt != th.Turns[0].Request.SystemPrompt {
		t.Fatalf("architect's thread: %+v", th)
	}
}

func TestArchitectAskingTurnThatFailsParksAndAnInterruptedAnswerIsRepeated(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	c.Release("1")
	f.Script(intake.DraftTurnID(1, 1), nil, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(intakefixture.Asks(p, "1")(ctx, req, verified, tools), errors.New("the agent crashed"))
	})
	started := make(chan struct{})
	answering := f.Answer("1", func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	f.Script(intake.DraftTurnID(1, 2), map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		for _, want := range []string{"The answers to the questions you asked in this draft:\n\nAnswer to your question 1.", "<<< osmia:answer", "| " + intakefixture.AskedAnswer} {
			if !strings.Contains(req.Prompt, want) {
				p.Report("the next attempt's prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		return nil
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	<-started
	f.Stop(t)
	f.Start(t)
	f.Await(t, stream, intakefixture.Sketched)
	p.Check(t)
	if moves, want := f.DraftMoves(t, stream), []string{"draft-1  -> drafting-1", "draft-1-waiting-1 drafting-1 -> waiting-1", "draft-1-resume-1 waiting-1 -> drafting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("draft went %v, want %v", moves, want)
	}
	if park := f.Transition(t, stream, "draft-1-waiting-1"); park.Reason != "draft 1 is parked: the architect waits for the answer to question 1" {
		t.Fatalf("park: %+v", park)
	}
	var statuses []string
	for _, q := range f.ArchitectThread(t, stream).Turns {
		statuses = append(statuses, q.Request.TurnID+" "+q.Status())
	}
	if want := []string{intake.DraftTurnID(1, 1) + " failed", answering + " interrupted", intake.DraftTurnID(1, 2) + " idle"}; !slices.Equal(statuses, want) {
		t.Fatalf("architect's turns %v, want %v", statuses, want)
	}
	if ops := f.AcknowledgedDraftOperations(t, stream); len(ops) != 2 || ops[0].Result.Outcome != questions.Waiting || ops[1].Result.Outcome != "succeeded" {
		t.Fatalf("draft operations: %v", intakefixture.Results(ops))
	}
}

func TestArchitectQuestionHoldsTheReplyAndTheConclusion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	f, _ := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	size := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	asking := intake.ReplyTurnID(1, 1)
	f.Script(asking, nil, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(intakefixture.Answers(p, "I will split it.", size)(ctx, req, verified, tools), intakefixture.Asks(p, "1")(ctx, req, verified, tools))
	})
	answering := f.Answer("1", func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if names, err := intakefixture.ToolNames(ctx, tools); err != nil || !slices.Equal(names, []string{questions.AskTool, intake.DraftTool, "file_read", "project_context", shed.ReplyTool}) {
			p.Report("answer turn tools %v %v", names, err)
		}
		return intakefixture.Delivers(p, plan.PlanPath, splitPlan, intake.DraftValid)(ctx, req, verified, tools)
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "asked-1", "replied-1", "failed-1")
	p.Check(t)

	ops := f.AcknowledgedReplies(t, stream)
	parked := "the reply to round 1 is parked: the architect waits for the answer to question 1"
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != questions.Waiting || ops[0].Result.Evidence != parked {
		t.Fatalf("reply operations: %v", intakefixture.Results(ops))
	}
	park := f.Transition(t, stream, "shed-reply-1-waiting-1")
	if park.From != "reply-1" || park.To != "asked-1" || park.Cause != ops[0].Operation.ID || park.Reason != parked || park.Actor != intake.ShedActor {
		t.Fatalf("park: %+v", park)
	}
	if docs := f.Documents(t, stream, shed.ReplyDocumentID(1)); len(docs) != 0 {
		t.Fatalf("a parked reply recorded %+v", docs)
	}

	if objected, err := f.C.ShedObject(ctx, stream, "The plan never names the retry budget."); err != nil || objected.Action != "objected" || objected.Round != 1 {
		t.Fatalf("owner objection while parked: %+v %v", objected, err)
	}
	f.AwaitQuestion(t, stream, "1", trace.QuestionEscalated)
	f.Stop(t)
	f.Start(t)
	fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).Pass(ctx))
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "asked-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v while parked, want %v", moves, want)
	}

	f.Rule(t, "1")
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "asked-1", "reply-1", "replied-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	resumption := f.Transition(t, stream, "shed-reply-1-resume-1")
	if resumption.From != "asked-1" || resumption.To != "reply-1" || resumption.Cause != "shed-reply-1-waiting-1" || resumption.Reason != "the reply to round 1 resumes: the architect's question is answered" {
		t.Fatalf("resumption: %+v", resumption)
	}
	ops = f.AcknowledgedReplies(t, stream)
	if len(ops) != 2 {
		t.Fatalf("reply operations: %v", intakefixture.Results(ops))
	}
	resumed := slices.IndexFunc(ops, func(o trace.OperationRecord) bool {
		in, err := intake.DecodeShed(o.Operation, intake.ReplyAction)
		return err == nil && in == (intake.RoundInput{Round: 1, Spec: 1, Plan: 1, Resume: 1})
	})
	if resumed < 0 || ops[resumed].Result.Outcome != "succeeded" {
		t.Fatalf("reply operations: %v", intakefixture.Results(ops))
	}
	if told := f.Transition(t, stream, "shed-reply-1-replied"); told.Cause != ops[resumed].Operation.ID || told.Reason != "the architect answered 1 objections after round 1 and redrafted: spec.md revision 1 and plan.json revision 2" {
		t.Fatalf("replied: %+v", told)
	}
	reply := f.Reply(t, stream, 1)
	if reply.Turn != answering || !slices.Equal(reply.Answers, []shed.Answer{{Objection: size, Answer: "I will split it."}}) || reply.Redraft == nil || *reply.Redraft != (shed.Pin{Spec: 1, Plan: 2}) {
		t.Fatalf("reply: %+v", reply)
	}
	if plans := f.Documents(t, stream, plan.PlanDocument); len(plans) != 2 || plans[1].Content != splitPlan {
		t.Fatalf("plans: %+v", plans)
	}
	if ran := f.Ran(); ran[asking] != 1 || ran[answering] != 1 || ran[intake.ReplyTurnID(1, 2)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
}

func TestArchitectQuestionParksTheRedraftUntilTheAnswerArrives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	f, _ := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	redraft := intake.RoundInput{Round: 1, Redraft: true}
	asking := redraft.TurnID(1)
	f.Script(asking, nil, intakefixture.Asks(p, "1"))
	answering := f.Answer("1", intakefixture.Delivers(p, plan.PlanPath, splitPlan, intake.DraftValid))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	if _, err := f.C.ShedRedraft(ctx, stream, "Split the resume unit."); err != nil {
		t.Fatal(err)
	}
	f.AwaitShed(t, stream, "asked-1", "redrafted-1", "failed-1")
	p.Check(t)
	redrafts := func() []trace.OperationRecord {
		return fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
			ops, err := f.Repository().Operations(stream)
			fixturekit.Must(t, err)
			return slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.Action != intake.RedraftAction })
		})
	}
	parked := "the redraft after round 1 is parked: the architect waits for the answer to question 1"
	ops := redrafts()
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != questions.Waiting || ops[0].Result.Evidence != parked {
		t.Fatalf("redraft operations: %v", intakefixture.Results(ops))
	}
	park := f.Transition(t, stream, "shed-redraft-1-waiting-1")
	if park.From != "redraft-1" || park.To != "asked-1" || park.Cause != ops[0].Operation.ID || park.Reason != parked || park.Actor != intake.ShedActor {
		t.Fatalf("park: %+v", park)
	}
	if docs := f.Documents(t, stream, shed.RedraftedDocumentID(1)); len(docs) != 0 {
		t.Fatalf("a parked redraft recorded %+v", docs)
	}

	edited := strings.Replace(fixturekit.ValidSpec, "never sent again", "never sent twice", 1)
	servicetest.EditDraft(t, f.C.Client, stream, edited, "")

	f.Rule(t, "1")
	f.AwaitShed(t, stream, "redrafted-1", "failed-1")
	p.Check(t)
	moves := f.ShedMoves(t, stream)
	if want := []string{"round-1", "heard-1", "reply-1", "replied-1", "concluded-1", "redraft-1", "asked-1", "redraft-1", "redrafted-1"}; len(moves) < len(want) || !slices.Equal(moves[:len(want)], want) {
		t.Fatalf("shed went %v, want %v first", moves, want)
	}
	resumption := f.Transition(t, stream, "shed-redraft-1-resume-1")
	if resumption.From != "asked-1" || resumption.To != "redraft-1" || resumption.Cause != "shed-redraft-1-waiting-1" || resumption.Reason != "the redraft after round 1 resumes: the architect's question is answered" {
		t.Fatalf("resumption: %+v", resumption)
	}
	ops = redrafts()
	resumed := slices.IndexFunc(ops, func(o trace.OperationRecord) bool {
		in, err := intake.DecodeShed(o.Operation, intake.RedraftAction)
		return err == nil && in == (intake.RoundInput{Round: 1, Spec: 1, Plan: 1, Resume: 1})
	})
	if len(ops) != 2 || resumed < 0 || ops[resumed].Result == nil || ops[resumed].Result.Outcome != "succeeded" {
		t.Fatalf("redraft operations: %v", intakefixture.Results(ops))
	}
	if told := f.Transition(t, stream, "shed-redraft-1-redrafted"); told.Cause != ops[resumed].Operation.ID {
		t.Fatalf("redrafted: %+v", told)
	}
	docs := f.Documents(t, stream, shed.RedraftedDocumentID(1))
	if len(docs) != 1 {
		t.Fatalf("redraft documents %+v", docs)
	}
	report, err := shed.ParseReply([]byte(docs[0].Content))
	fixturekit.Must(t, err)
	if report.Turn != answering || report.Revision != (shed.Pin{Spec: 1, Plan: 1}) || report.Redraft == nil || *report.Redraft != (shed.Pin{Spec: 2, Plan: 2}) {
		t.Fatalf("the architect's redraft %+v", report)
	}
	if ran := f.Ran(); ran[asking] != 1 || ran[answering] != 1 || ran[redraft.TurnID(2)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
}

func TestOwnerSkipsDebateWhileTheReplyIsParked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	f, _ := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Asks(p, "1"))
	answering := f.Answer("1", intakefixture.Silent)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "asked-1", "replied-1", "failed-1")
	p.Check(t)
	if skipped, err := f.C.ShedSkip(ctx, stream); err != nil || skipped.Action != intake.SkippedValue {
		t.Fatalf("skip while the reply is parked: %+v %v", skipped, err)
	}
	f.Rule(t, "1")
	queued := f.AwaitTurn(t, stream, intake.ArchitectAgent, answering)
	awaitDocument(t, f, stream, shed.PacketDocumentID(1))
	fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).Pass(ctx))
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "asked-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if ran := f.Ran(); ran[answering] != 0 || !queued.CompletedAt.IsZero() || len(f.ReplyOperations(t, stream)) != 1 {
		t.Fatalf("a skipped debate ran the answer: %v", ran)
	}
	f.StillInShed(t, stream)
}

func TestAbandoningWhileTheArchitectAsksFailsTheDraft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	f, _ := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	asked := make(chan struct{})
	f.Script(intake.DraftTurnID(1, 1), nil, intakefixture.AsksThenWaits(p, asked))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	<-asked
	if _, err := f.C.Abandon(ctx, stream, "Superseded."); err != nil {
		t.Fatal(err)
	}
	f.Await(t, stream, intakefixture.DraftAt("failed-1"))
	p.Check(t)
	if moves, want := f.DraftMoves(t, stream), []string{"draft-1  -> drafting-1", "draft-1-failed drafting-1 -> failed-1"}; !slices.Equal(moves, want) {
		t.Fatalf("draft went %v, want %v", moves, want)
	}
	if told := f.Transition(t, stream, "draft-1-failed"); told.Reason != "draft 1 failed: the workstream was abandoned, so the architect runs no turn for it" {
		t.Fatalf("failed: %+v", told)
	}
}

func TestAbandoningWhileTheArchitectAsksFailsTheReply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	f, _ := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	asked := make(chan struct{})
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.AsksThenWaits(p, asked))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	<-asked
	if _, err := f.C.Abandon(ctx, stream, "Superseded."); err != nil {
		t.Fatal(err)
	}
	f.AwaitShed(t, stream, "failed-1", "asked-1", "replied-1")
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "failed-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if told := f.Transition(t, stream, "shed-reply-1-failed"); told.Reason != "the reply to round 1 failed: the workstream was abandoned, so the architect's reply is not recorded" {
		t.Fatalf("failed: %+v", told)
	}
}

func TestInterruptedReplyAnswerIsRepeatedToTheNextAttempt(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	c.Release("1")
	f.Member(1, 1, 1, intakefixture.Objects(p, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Asks(p, "1"))
	started := make(chan struct{})
	answering := f.Answer("1", func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	f.Script(intake.ReplyTurnID(1, 2), nil, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		for _, want := range []string{"The answers to the questions you asked in this reply:\n\nAnswer to your question 1.", "<<< osmia:answer", "| " + intakefixture.AskedAnswer} {
			if !strings.Contains(req.Prompt, want) {
				p.Report("the next attempt's prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		return nil
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	<-started
	f.Stop(t)
	f.Start(t)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	var statuses []string
	for _, q := range f.ArchitectThread(t, stream).Turns {
		statuses = append(statuses, q.Request.TurnID+" "+q.Status())
	}
	if want := []string{intake.DraftTurnID(1, 1) + " idle", intake.ReplyTurnID(1, 1) + " waiting", answering + " interrupted", intake.ReplyTurnID(1, 2) + " idle"}; !slices.Equal(statuses, want) {
		t.Fatalf("architect's turns %v, want %v", statuses, want)
	}
	if reply := f.Reply(t, stream, 1); reply.Turn != intake.ReplyTurnID(1, 2) {
		t.Fatalf("reply: %+v", reply)
	}
}
