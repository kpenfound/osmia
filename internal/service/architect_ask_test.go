package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// delivers makes the architect's turn deliver one draft file.
func delivers(p *faults, path, content string) fakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if got, err := callTool(ctx, tools, DraftTool, map[string]any{"path": path, "content": content}); err != nil || got != `{}` {
			p.report("deliver %s: %q %v", path, got, err)
		}
		return nil
	}
}

// draftMoves lists the states the workstream's drafts went through, by
// transition.
func (f *shedFixture) draftMoves(t *testing.T, stream config.WorkstreamID) []string {
	t.Helper()
	var moves []string
	for _, tr := range f.transitions(t, stream) {
		if tr.Subject == draftSubject {
			moves = append(moves, tr.ID+" "+tr.From+" -> "+tr.To)
		}
	}
	return moves
}

// An architect that asks during draft 1 parks the draft: its turn ends
// waiting, the draft operation ends waiting and the workstream stays handed
// with no draft recorded, across a restart. The owner's ruling, relayed by the
// chief of staff, runs as the architect's next turn, which adds to what the
// asking turn delivered, and draft 1 is sketched without spending a draft or
// an attempt.
func TestArchitectQuestionParksTheDraftUntilTheAnswerArrives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &faults{}
	f, _ := newAskingFixture(t, 1, 1, p)
	defer f.stop(t)
	asking := draftTurnID(1, 1)
	f.script(asking, nil, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, "Call ask when something you must know to draft is not in your view") || !strings.Contains(req.SystemPrompt, "call ask: your work waits for the answer") {
			p.report("draft prompts do not offer ask:\n%s\n%s", req.SystemPrompt, req.Prompt)
		}
		return errors.Join(delivers(p, plan.SpecPath, validSpec)(ctx, req, verified, tools), asks(p, "1")(ctx, req, verified, tools))
	})
	answering := f.answer("1", func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		for _, want := range []string{"The owner ruled on your question 1. The chief of staff relays the ruling.", "You asked:\n" + askedQuestion, "Answer:\n" + relayedRuling} {
			if !strings.Contains(req.Prompt, want) {
				p.report("answer prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if !strings.Contains(req.SystemPrompt, "architect of the dagger project") || req.Profile.Name != architectRole {
			p.report("answer turn system prompt %q, profile %+v", req.SystemPrompt, req.Profile)
		}
		if names, err := toolNames(ctx, tools); err != nil || !slices.Equal(names, []string{questions.AskTool, DraftTool, "file_read"}) {
			p.report("answer turn tools %v %v", names, err)
		}
		return delivers(p, plan.PlanPath, validPlan)(ctx, req, verified, tools)
	})
	stream := f.handIn(t, "design", handedDesign)
	f.await(t, stream, draftAt("waiting-1"))
	p.check(t)

	// The draft operation ended waiting and the park is recorded with it.
	ops := f.acknowledgedDraftOperations(t, stream)
	parked := "draft 1 is parked: the architect waits for the answer to question 1"
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != questions.Waiting || ops[0].Result.Evidence != parked {
		t.Fatalf("draft operations: %v", results(ops))
	}
	park := f.transition(t, stream, "draft-1-waiting-1")
	if park.From != "drafting-1" || park.To != "waiting-1" || park.Cause != ops[0].Operation.ID || park.Reason != parked || park.Actor != draftingActor {
		t.Fatalf("park: %+v", park)
	}
	// The architect's turn ended waiting and its thread parked. Nothing is
	// recorded and the workstream stays handed.
	th := f.architectThread(t, stream)
	if last := th.Turns[len(th.Turns)-1]; !th.Parked() || last.Request.TurnID != asking || last.Status() != "waiting" || last.Response.Result.Outcome == nil || *last.Response.Result.Outcome != (coreadapter.Outcome{Status: "waiting", Report: "Asked question 1"}) {
		t.Fatalf("architect's thread: %+v", th)
	}
	if q := f.question(t, stream, "1"); q.Asked.AskedBy.ID != architectAgent || q.Asked.Thread != architectThread || q.Asked.Turn != asking || q.Asked.Question != askedQuestion {
		t.Fatalf("question: %+v", q)
	}
	if specs, plans := f.documents(t, stream, plan.SpecDocument), f.documents(t, stream, plan.PlanDocument); len(specs) != 0 || len(plans) != 0 {
		t.Fatalf("a parked draft recorded %+v %+v", specs, plans)
	}
	if state, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || state.Value != HandedState {
		t.Fatalf("feature %+v %v", state, err)
	}

	// The question reaches the owner's inbox through the chief of staff, and
	// a restart keeps the draft parked: the pass after it requests nothing.
	f.awaitQuestion(t, stream, "1", trace.QuestionEscalated)
	f.stop(t)
	f.start(t)
	if state, err := f.repository().Workflow(stream, draftSubject); err != nil || state.Value != "waiting-1" {
		t.Fatalf("draft after the restart: %+v %v", state, err)
	}
	must(t, (&drafter{s: f.s, repository: f.repository()}).Pass(ctx))
	if moves, want := f.draftMoves(t, stream), []string{"draft-1  -> drafting-1", "draft-1-waiting-1 drafting-1 -> waiting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("draft went %v after the restart, want %v", moves, want)
	}

	// The owner rules; the relayed ruling runs as the architect's next turn
	// and draft 1 is sketched from both turns' files.
	f.rule(t, "1")
	f.await(t, stream, sketched)
	p.check(t)
	if moves, want := f.draftMoves(t, stream), []string{"draft-1  -> drafting-1", "draft-1-waiting-1 drafting-1 -> waiting-1", "draft-1-resume-1 waiting-1 -> drafting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("draft went %v, want %v", moves, want)
	}
	resumption := f.transition(t, stream, "draft-1-resume-1")
	if resumption.Cause != "draft-1-waiting-1" || resumption.Actor != draftingActor || resumption.Reason != "draft 1 resumes: the architect's question is answered" {
		t.Fatalf("resumption: %+v", resumption)
	}
	ops = f.acknowledgedDraftOperations(t, stream)
	if len(ops) != 2 || ops[1].Result == nil || ops[1].Result.Outcome != "succeeded" {
		t.Fatalf("draft operations: %v", results(ops))
	}
	var in draftInput
	if err := json.Unmarshal(ops[1].Operation.Input, &in); err != nil || in != (draftInput{Draft: 1, Resume: 1}) {
		t.Fatalf("resumption input %+v %v", in, err)
	}
	if ops[1].Operation.ID != trace.OperationID(f.project, stream, trace.EventID("draft-1-resume-1", "run")) {
		t.Fatalf("resumption operation %s", ops[1].Operation.ID)
	}
	if told := f.transition(t, stream, SketchedState); told.Cause != ops[1].Operation.ID || !strings.HasPrefix(told.Reason, "the architect's draft 1 passed validation: spec.md revision 1") {
		t.Fatalf("sketched: %+v", told)
	}
	specs, plans := f.documents(t, stream, plan.SpecDocument), f.documents(t, stream, plan.PlanDocument)
	if len(specs) != 1 || specs[0].Content != validSpec || specs[0].Cause != ops[1].Operation.ID || len(plans) != 1 || plans[0].Content != validPlan {
		t.Fatalf("draft documents %+v %+v", specs, plans)
	}
	if ran := f.ran(); ran[asking] != 1 || ran[answering] != 1 || ran[draftTurnID(1, 2)] != 0 || ran[draftTurnID(2, 1)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	th = f.architectThread(t, stream)
	if th.Parked() || len(th.Turns) < 2 || th.Turns[1].Request.TurnID != answering || th.Turns[1].Status() != "idle" || th.Turns[1].Request.SystemPrompt != th.Turns[0].Request.SystemPrompt {
		t.Fatalf("architect's thread: %+v", th)
	}
}

// A draft turn that asks and then fails parks the draft all the same: the
// answer arrives on the thread the question was asked on. An answer turn a
// service stop interrupts is followed by the draft's next attempt, whose
// prompt repeats the answer, and the draft is still draft 1.
func TestArchitectAskingTurnThatFailsParksAndAnInterruptedAnswerIsRepeated(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.stop(t)
	c.release("1")
	f.script(draftTurnID(1, 1), nil, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(asks(p, "1")(ctx, req, verified, tools), errors.New("the agent crashed"))
	})
	started := make(chan struct{})
	answering := f.answer("1", func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	f.script(draftTurnID(1, 2), map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		for _, want := range []string{"The answers to the questions you asked in this draft:\n\nAnswer to your question 1.", "Answer:\n" + askedAnswer} {
			if !strings.Contains(req.Prompt, want) {
				p.report("the next attempt's prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		return nil
	})
	stream := f.handIn(t, "design", handedDesign)
	<-started
	f.stop(t)
	f.start(t)
	f.await(t, stream, sketched)
	p.check(t)
	if moves, want := f.draftMoves(t, stream), []string{"draft-1  -> drafting-1", "draft-1-waiting-1 drafting-1 -> waiting-1", "draft-1-resume-1 waiting-1 -> drafting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("draft went %v, want %v", moves, want)
	}
	if park := f.transition(t, stream, "draft-1-waiting-1"); park.Reason != "draft 1 is parked: the architect waits for the answer to question 1" {
		t.Fatalf("park: %+v", park)
	}
	var statuses []string
	for _, q := range f.architectThread(t, stream).Turns {
		statuses = append(statuses, q.Request.TurnID+" "+q.Status())
	}
	if want := []string{draftTurnID(1, 1) + " failed", answering + " interrupted", draftTurnID(1, 2) + " idle"}; !slices.Equal(statuses, want) {
		t.Fatalf("architect's turns %v, want %v", statuses, want)
	}
	if ops := f.acknowledgedDraftOperations(t, stream); len(ops) != 2 || ops[0].Result.Outcome != questions.Waiting || ops[1].Result.Outcome != "succeeded" {
		t.Fatalf("draft operations: %v", results(ops))
	}
}

// An architect that asks during its reply to a round parks the reply: the
// shed moves to asked-<n> and the debate does not conclude, across a restart
// and through the owner's actions. The answer runs as the architect's next
// turn, which continues the reply: the answer given before asking is kept,
// the redraft it delivers is recorded, and only then does the debate
// conclude.
func TestArchitectQuestionHoldsTheReplyAndTheConclusion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &faults{}
	f, _ := newAskingFixture(t, 1, 1, p)
	defer f.stop(t)
	size := shed.ObjectionID(1, committeeAgent(1), 1)
	f.member(1, 1, 1, objects(p, shed.Size, "plan#resume", "spec#1"))
	asking := replyTurnID(1, 1)
	f.script(asking, nil, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(answers(p, "I will split it.", size)(ctx, req, verified, tools), asks(p, "1")(ctx, req, verified, tools))
	})
	answering := f.answer("1", func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if names, err := toolNames(ctx, tools); err != nil || !slices.Equal(names, []string{questions.AskTool, DraftTool, "file_read", shed.ReplyTool}) {
			p.report("answer turn tools %v %v", names, err)
		}
		return delivers(p, plan.PlanPath, splitPlan)(ctx, req, verified, tools)
	})
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "asked-1", "replied-1", "failed-1")
	p.check(t)

	ops := f.acknowledgedReplies(t, stream)
	parked := "the reply to round 1 is parked: the architect waits for the answer to question 1"
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != questions.Waiting || ops[0].Result.Evidence != parked {
		t.Fatalf("reply operations: %v", results(ops))
	}
	park := f.transition(t, stream, "shed-reply-1-waiting-1")
	if park.From != "reply-1" || park.To != "asked-1" || park.Cause != ops[0].Operation.ID || park.Reason != parked || park.Actor != shedActor {
		t.Fatalf("park: %+v", park)
	}
	if docs := f.documents(t, stream, shed.ReplyDocumentID(1)); len(docs) != 0 {
		t.Fatalf("a parked reply recorded %+v", docs)
	}

	// The owner objects while the reply is parked, and a restart keeps it
	// parked: the pass after it requests nothing and concludes nothing.
	if objected, err := f.c.ShedObject(ctx, stream, "The plan never names the retry budget."); err != nil || objected.Action != "objected" || objected.Round != 1 {
		t.Fatalf("owner objection while parked: %+v %v", objected, err)
	}
	f.awaitQuestion(t, stream, "1", trace.QuestionEscalated)
	f.stop(t)
	f.start(t)
	must(t, (&debate{s: f.s, repository: f.repository()}).Pass(ctx))
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "asked-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v while parked, want %v", moves, want)
	}

	f.rule(t, "1")
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "heard-1", "reply-1", "asked-1", "reply-1", "replied-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	resumption := f.transition(t, stream, "shed-reply-1-resume-1")
	if resumption.From != "asked-1" || resumption.To != "reply-1" || resumption.Cause != "shed-reply-1-waiting-1" || resumption.Reason != "the reply to round 1 resumes: the architect's question is answered" {
		t.Fatalf("resumption: %+v", resumption)
	}
	ops = f.acknowledgedReplies(t, stream)
	if len(ops) != 2 {
		t.Fatalf("reply operations: %v", results(ops))
	}
	resumed := slices.IndexFunc(ops, func(o trace.OperationRecord) bool {
		in, err := decodeShed(o.Operation, ReplyAction)
		return err == nil && in == (roundInput{Round: 1, Spec: 1, Plan: 1, Resume: 1})
	})
	if resumed < 0 || ops[resumed].Result.Outcome != "succeeded" {
		t.Fatalf("reply operations: %v", results(ops))
	}
	if told := f.transition(t, stream, "shed-reply-1-replied"); told.Cause != ops[resumed].Operation.ID || told.Reason != "the architect answered 1 objections after round 1 and redrafted: spec.md revision 1 and plan.json revision 2" {
		t.Fatalf("replied: %+v", told)
	}
	reply := f.reply(t, stream, 1)
	if reply.Turn != answering || !slices.Equal(reply.Answers, []shed.Answer{{Objection: size, Answer: "I will split it."}}) || reply.Redraft == nil || *reply.Redraft != (shed.Pin{Spec: 1, Plan: 2}) {
		t.Fatalf("reply: %+v", reply)
	}
	if plans := f.documents(t, stream, plan.PlanDocument); len(plans) != 2 || plans[1].Content != splitPlan {
		t.Fatalf("plans: %+v", plans)
	}
	if ran := f.ran(); ran[asking] != 1 || ran[answering] != 1 || ran[replyTurnID(1, 2)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
}

func TestDraftOperationInputIsValidated(t *testing.T) {
	t.Parallel()
	for name, op := range map[string]coreadapter.Operation{
		"another action":        {Boundary: coreadapter.RunnerBoundary, Action: ReplyAction, Input: json.RawMessage(`{"draft":1}`)},
		"unknown field":         {Boundary: coreadapter.RunnerBoundary, Action: DraftAction, Input: json.RawMessage(`{"draft":1,"extra":1}`)},
		"no draft":              {Boundary: coreadapter.RunnerBoundary, Action: DraftAction, Input: json.RawMessage(`{"draft":0}`)},
		"a negative resumption": {Boundary: coreadapter.RunnerBoundary, Action: DraftAction, Input: json.RawMessage(`{"draft":1,"resume":-1}`)},
	} {
		if _, err := decodeDraft(op); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if in, err := decodeDraft(coreadapter.Operation{Boundary: coreadapter.RunnerBoundary, Action: DraftAction, Input: json.RawMessage(`{"draft":2,"resume":1}`)}); err != nil || in != (draftInput{Draft: 2, Resume: 1}) {
		t.Fatalf("a resumption: %+v %v", in, err)
	}
}
