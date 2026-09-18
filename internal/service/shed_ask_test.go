package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	askedQuestion = "Does the store keep acknowledged chunks across a restart?"
	askedAnswer   = "Yes: the store is a file under the root, and files survive restarts."
	// chiefRole runs the chief of staff in a container, like every other role
	// of the shed fixtures.
	chiefRole = "[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n"
	// pinOne names the revision the fixtures' round 1 is pinned to.
	pinOne = "spec.md revision 1 and plan.json revision 1"
)

// newAskingFixture is a debate fixture whose service also runs the chief of
// staff, so a member's question is answered and the answer delivered. The
// fake chief of staff answers question 1 once release is closed, citing the
// charter, and does nothing for any other event.
func newAskingFixture(t *testing.T, members, rounds int, p *faults, release <-chan struct{}) *shedFixture {
	t.Helper()
	f := newDebateFixtureWith(t, members, rounds, chiefRole, func(opts *Options) {
		opts.Threads = Enforce(*opts, Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts}).Threads
	})
	var mu sync.Mutex
	answered := false
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		done := answered
		mu.Unlock()
		if done || !strings.Contains(req.Prompt, "Question 1 is open, asked by the committee: "+askedQuestion) {
			return questionResult(req, "session-chief", "Noted"), nil
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		got, err := callTool(ctx, tools, questions.AnswerTool, map[string]any{"question": "1", "text": askedAnswer, "citations": []string{"charter#1"}})
		if err != nil || !strings.Contains(got, `"recorded":true`) {
			p.report("the chief of staff's answer: %q %v", got, err)
		}
		mu.Lock()
		answered = true
		mu.Unlock()
		return questionResult(req, "session-chief", "Answered"), nil
	}
	return f
}

// answer installs the fake behaviour of the turn that delivers the answer to
// question id to its asker.
func (f *shedFixture) answer(id string, run fakeTurn) string {
	turn := questions.TurnID(id)
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns[turn] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if err := run(ctx, req, verified, tools); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + turn, ResultText: "Answer read", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
	return turn
}

// asks makes a member's turn ask question 1 and check the tool's answer.
func asks(p *faults) fakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		got, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": askedQuestion})
		if err != nil || got != `{"recorded":true,"question":"1","next":"End your turn now. The answer arrives as your next turn on this thread."}` {
			p.report("ask: %q %v", got, err)
		}
		if got, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "And another?"}); err != nil || !strings.Contains(got, "this turn already asked question 1") {
			p.report("a second question in the turn: %q %v", got, err)
		}
		return nil
	}
}

func (f *shedFixture) thread(t *testing.T, stream config.WorkstreamID, member string) trace.Thread {
	t.Helper()
	th, err := f.repository().Thread(stream, member)
	must(t, err)
	return th
}

// awaitTurn waits until the member's thread holds the given turn.
func (f *shedFixture) awaitTurn(t *testing.T, stream config.WorkstreamID, member, turn string) trace.QueuedTurn {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		th := f.thread(t, stream, member)
		if i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == turn }); i >= 0 {
			return th.Turns[i]
		}
		if time.Now().After(deadline) {
			t.Fatalf("thread of %s never held turn %s: %+v", member, turn, th.Turns)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A member that asks parks the round: its turn ends waiting, the round
// operation ends waiting, no reply is requested and the workstream stays in
// the shed, across a restart and through the owner's actions. The answer,
// once the chief of staff records it, runs as the member's next turn against
// the same pinned revision, continuing what it contributed before asking, and
// the round is heard with every member.
func TestMemberQuestionParksTheRoundUntilTheAnswerArrives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &faults{}
	release := make(chan struct{})
	f := newAskingFixture(t, 2, 1, p, release)
	defer f.stop(t)
	member := committeeAgent(1)
	first, second := shed.ObjectionID(1, member, 1), shed.ObjectionID(1, member, 2)
	asking := f.member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(objects(p, shed.Fit, "plan", "spec#1")(ctx, req, verified, tools), asks(p)(ctx, req, verified, tools))
	})
	other := f.member(1, 2, 1, silent)
	answering := f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, want := range []string{"Answer to your question 1.", "You asked:\n" + askedQuestion, "Answer:\n" + askedAnswer, "Citations:\n- charter#1"} {
			if !strings.Contains(req.Prompt, want) {
				p.report("answer prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if !strings.Contains(req.SystemPrompt, "member of the committee") || req.Profile.Name != committeeRole {
			p.report("answer turn system prompt %q, profile %+v", req.SystemPrompt, req.Profile)
		}
		names, err := toolNames(ctx, tools)
		if err != nil || !slices.Equal(names, []string{questions.AskTool, shed.ConcedeTool, "file_read", shed.ObjectTool}) {
			p.report("answer turn tools %v %v", names, err)
		}
		// The owner edited the spec while the round was parked; the round
		// stays pinned to the revision it debates.
		if got, err := readTool(ctx, tools, "spec.md"); err != nil || got != validSpec {
			p.report("the answer turn read spec %q %v", got, err)
		}
		// The objection made before asking is kept, so the next one is the
		// second, and both can be conceded.
		if recorded, id, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "proof", "part": "spec#2", "argument": "No unit shows it.", "citations": []string{"plan#dedupe"}}); err != nil || !recorded || id != second {
			p.report("objection after the answer: %v %q %v", recorded, id, err)
		}
		if err := concedes(p, first, second)(ctx, req, nil, tools); err != nil {
			return err
		}
		return nil
	})
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "waiting-1", "heard-1", "failed-1")
	p.check(t)

	// The round operation ended waiting and the park is recorded with it.
	ops := f.acknowledgedRoundOperations(t, stream)
	parked := fmt.Sprintf("round 1 against %s is parked: %s waits for the answer to question 1", pinOne, member)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "waiting" || ops[0].Result.Evidence != parked {
		t.Fatalf("round operations: %v", results(ops))
	}
	park := f.transition(t, stream, "shed-round-1-waiting-1")
	if park.From != "round-1" || park.To != "waiting-1" || park.Cause != ops[0].Operation.ID || park.Reason != parked || park.Actor != shedActor {
		t.Fatalf("park: %+v", park)
	}
	// The asker's turn ended waiting and its thread parked; the other member
	// finished normally. No slot is held and nothing is recorded.
	asker := f.thread(t, stream, member)
	if last := asker.Turns[len(asker.Turns)-1]; !asker.Parked() || last.Request.TurnID != asking || last.Status() != "waiting" || last.Response.Result.Outcome == nil || *last.Response.Result.Outcome != (coreadapter.Outcome{Status: "waiting", Report: "Asked question 1"}) {
		t.Fatalf("asker's thread: %+v", asker)
	}
	if th := f.thread(t, stream, committeeAgent(2)); th.Parked() || th.Turns[len(th.Turns)-1].Status() != "idle" {
		t.Fatalf("other member's thread: %+v", th)
	}
	asked, err := f.repository().Questions(stream)
	must(t, err)
	if len(asked) != 1 || asked[0].State != trace.QuestionOpen || asked[0].Asked.AskedBy.ID != member || asked[0].Asked.Turn != asking || asked[0].Asked.Question != askedQuestion {
		t.Fatalf("questions: %+v", asked)
	}
	if records, err := shed.Records(f.repository(), stream); err != nil || len(records) != 0 {
		t.Fatalf("records of a parked round: %+v %v", records, err)
	}
	if len(f.replyOperations(t, stream)) != 0 {
		t.Fatal("the architect was asked for a reply while the round is parked")
	}
	f.stillInShed(t, stream)

	// The owner objects and rules while the round is parked.
	objected, err := f.c.ShedObject(ctx, stream, "The plan never names the retry budget.")
	if err != nil || objected.Action != "objected" || objected.Round != 1 {
		t.Fatalf("owner objection while parked: %+v %v", objected, err)
	}
	if ruled, err := f.c.ShedRule(ctx, stream, objected.Objection, "dismiss", "The budget is the default."); err != nil || ruled.Action != "dismissed" {
		t.Fatalf("owner ruling while parked: %+v %v", ruled, err)
	}
	edited := strings.Replace(validSpec, "never sent again", "never sent twice", 1)
	must(t, os.WriteFile(filepath.Join(f.trace, "workstreams", string(stream), plan.SpecPath), []byte(edited), 0600))

	// A restart keeps the round parked: the pass after it requests nothing.
	f.stop(t)
	f.start(t)
	if state, err := f.repository().Workflow(stream, shedSubject); err != nil || state.Value != "waiting-1" {
		t.Fatalf("shed after the restart: %+v %v", state, err)
	}
	must(t, (&debate{s: f.s, repository: f.repository()}).Pass(ctx))
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "waiting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v after the restart, want %v", moves, want)
	}

	// The chief of staff answers; the answer runs as the member's next turn
	// and the round is heard with both members.
	close(release)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	ops = f.acknowledgedRoundOperations(t, stream)
	slices.SortFunc(ops, func(a, b trace.OperationRecord) int { return strings.Compare(a.Result.Outcome, b.Result.Outcome) })
	// The owner's objection is the one that stands in the record; the ruling
	// that dismissed it is read at the conclusion.
	heard := fmt.Sprintf("round 1 against %s: 2 members heard, 2 objections, 2 concessions, 0 failed turns; 1 objections stand", pinOne)
	if len(ops) != 2 || ops[0].Result.Outcome != "succeeded" || ops[0].Result.Evidence != heard || ops[1].Result.Outcome != "waiting" || !ops[0].Acknowledged || !ops[1].Acknowledged {
		t.Fatalf("round operations: %v", results(ops))
	}
	if in, err := decodeRound(ops[0].Operation); err != nil || in != (roundInput{Round: 1, Spec: 1, Plan: 1, Resume: 1}) {
		t.Fatalf("resumption input %+v %v", in, err)
	}
	resumed := f.transition(t, stream, "shed-round-1-resume-1")
	if resumed.From != "waiting-1" || resumed.To != "round-1" || resumed.Cause != "shed-round-1-waiting-1" || resumed.Actor != shedActor || resumed.Reason != fmt.Sprintf("round 1 against %s resumes: every member that asked has its answer", pinOne) {
		t.Fatalf("resumption: %+v", resumed)
	}
	if told := f.transition(t, stream, "shed-round-1-heard"); told.Cause != ops[0].Operation.ID || told.Reason != heard {
		t.Fatalf("heard: %+v", told)
	}
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	var mine shed.Record
	for _, r := range records {
		if r.Member == member {
			mine = r
		}
	}
	if len(records) != 3 || mine.Turn != asking || mine.Failure != "" || len(mine.Objections) != 2 || mine.Objections[1].ID != second ||
		!slices.Equal(mine.Concessions, []shed.Concession{{Objection: first, Reason: "The reply settles it."}, {Objection: second, Reason: "The reply settles it."}}) {
		t.Fatalf("records: %+v", records)
	}
	if end := f.transition(t, stream, "shed-concluded-1"); end.Reason != "debate concluded after round 1: the owner disposed of every objection that stood" {
		t.Fatalf("conclusion: %+v", end)
	}
	if ran := f.ran(); ran[asking] != 1 || ran[other] != 1 || ran[answering] != 1 || ran[roundTurnID(1, member, 2)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	asked, err = f.repository().Questions(stream)
	must(t, err)
	if len(asked) != 1 || asked[0].State != trace.QuestionAnswered {
		t.Fatalf("questions: %+v", asked)
	}
	if th := f.thread(t, stream, member); th.Parked() || len(th.Turns) != 2 || th.Turns[1].Request.TurnID != answering || th.Turns[1].Status() != "idle" || th.Turns[1].Request.SystemPrompt != th.Turns[0].Request.SystemPrompt {
		t.Fatalf("asker's thread: %+v", th)
	}
	if specs := f.documents(t, stream, plan.SpecDocument); len(specs) != 2 || specs[1].Content != edited {
		t.Fatalf("the owner's edit while parked was not recorded: %+v", specs)
	}
	f.stillInShed(t, stream)
	root := f.opts.Config.Root
	if _, err := os.Lstat(filepath.Join(root, "shed", string(f.project), string(stream), answering, "workspace")); !os.IsNotExist(err) {
		t.Fatal("the answer turn's staged workspace is retained")
	}
	if entries, err := os.ReadDir(filepath.Join(root, "views")); err != nil || len(entries) != 0 {
		t.Fatalf("views leaked: %v %v", entries, err)
	}
}

// The owner may skip debate while a round is parked: the skip is recorded,
// the packet is presented, and the answer, once delivered, runs no turn.
func TestOwnerSkipsDebateWhileARoundIsParked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &faults{}
	release := make(chan struct{})
	f := newAskingFixture(t, 1, 1, p, release)
	defer f.stop(t)
	member := committeeAgent(1)
	f.member(1, 1, 1, asks(p))
	answering := f.answer("1", silent)
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "waiting-1", "heard-1", "failed-1")
	p.check(t)
	if skipped, err := f.c.ShedSkip(ctx, stream); err != nil || skipped.Action != skippedValue {
		t.Fatalf("skip while parked: %+v %v", skipped, err)
	}
	close(release)
	// The answer is queued on the member's thread and the packet presented,
	// so the pass after both requests nothing for the round.
	queued := f.awaitTurn(t, stream, member, answering)
	awaitDocument(t, f, stream, shed.PacketDocumentID(1))
	must(t, (&debate{s: f.s, repository: f.repository()}).Pass(ctx))
	p.check(t)
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "waiting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if ran := f.ran(); ran[answering] != 0 || !queued.CompletedAt.IsZero() || len(f.roundOperations(t, stream)) != 1 {
		t.Fatalf("a skipped debate ran the answer: %v", ran)
	}
	if !skippedOn(t, f, stream) {
		t.Fatal("the skip is not recorded")
	}
	f.stillInShed(t, stream)
}

// results renders acknowledged operations with their results, which %v
// prints as pointers.
func results(ops []trace.OperationRecord) []string {
	var out []string
	for _, o := range ops {
		result := "no result"
		if o.Result != nil {
			result = fmt.Sprintf("%+v", *o.Result)
		}
		out = append(out, fmt.Sprintf("%s acknowledged %t: %s", o.Operation.ID, o.Acknowledged, result))
	}
	return out
}

func skippedOn(t *testing.T, f *shedFixture, stream config.WorkstreamID) bool {
	t.Helper()
	skipped, err := skippedDebate(f.repository(), stream)
	must(t, err)
	return skipped
}

// awaitDocument waits until the workstream records the document.
func awaitDocument(t *testing.T, f *shedFixture, stream config.WorkstreamID, id string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for len(f.documents(t, stream, id)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("document %s was never recorded", id)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A service stop that interrupts the answer's turn is recovered like any
// other: the member's next attempt carries the answer it received, and the
// round is heard with what that attempt contributed.
func TestInterruptedAnswerTurnIsRetriedWithTheAnswer(t *testing.T) {
	t.Parallel()
	p := &faults{}
	release := make(chan struct{})
	close(release)
	f := newAskingFixture(t, 1, 1, p, release)
	defer f.stop(t)
	member := committeeAgent(1)
	asking := f.member(1, 1, 1, asks(p))
	started := make(chan struct{})
	var once sync.Once
	answering := f.answer("1", func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	})
	retried := f.member(1, 1, 2, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		for _, want := range []string{"Round 1 of the shed", "The answers to the questions you asked in this round:", "Answer to your question 1.", "You asked:\n" + askedQuestion, "Answer:\n" + askedAnswer} {
			if !strings.Contains(req.Prompt, want) {
				p.report("retried attempt's prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		return nil
	})
	stream := f.handIn(t, "design", handedDesign)
	select {
	case <-started:
	case <-time.After(demoTimeout):
		t.Fatal("the answer's turn never started")
	}
	f.stop(t)
	f.start(t)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if ran := f.ran(); ran[asking] != 1 || ran[answering] != 1 || ran[retried] != 1 {
		t.Fatalf("turns ran: %v", ran)
	}
	th := f.thread(t, stream, member)
	if len(th.Turns) != 3 || th.Turns[1].Request.TurnID != answering || th.Turns[1].Status() != "interrupted" || th.Turns[2].Request.TurnID != retried || th.Turns[2].Status() != "idle" {
		t.Fatalf("asker's thread: %+v", th)
	}
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	if len(records) != 1 || records[0].Turn != retried || !records[0].Silent() {
		t.Fatalf("records: %+v", records)
	}
	if end := f.transition(t, stream, "shed-concluded-1"); end.Reason != "debate concluded by consensus after round 1: no objection stands" {
		t.Fatalf("conclusion: %+v", end)
	}
}
