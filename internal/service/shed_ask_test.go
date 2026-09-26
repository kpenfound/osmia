package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
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
	ownerRuling   = "Yes, the store is durable; do not object on that account."
	relayedRuling = "The store keeps acknowledged chunks across a restart: it is durable."
	// chiefRole runs the chief of staff in a container, like every other role
	// of the shed fixtures.
	chiefRole = "[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n"
	// pinOne names the revision the fixtures' round 1 is pinned to.
	pinOne = "spec.md revision 1 and plan.json revision 1"
)

var (
	openQuestion = regexp.MustCompile(`Question (\d+) is open, asked by the (?:committee|architect|mason|reviewer): `)
	ownerRuled   = regexp.MustCompile(`The owner ruled on inbox entry \d+, \S+ \(questions ([^)]+)\): `)
)

// chief is the fake chief of staff of the asking fixtures. A committee
// question it was told to release it answers from the record, citing the
// charter; one it was told to hold it answers the same way and then keeps the
// turn open until the hold closes or the turn is cancelled; any other it
// escalates to the owner, one entry per question, and relays the owner's
// ruling with local scope. Every other event it notes and leaves.
type chief struct {
	p        *faults
	mu       sync.Mutex
	released map[string]bool
	held     map[string]chan struct{}
}

// release has the chief of staff answer question id from the record.
func (c *chief) release(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released[id] = true
}

// hold has the chief of staff answer question id from the record and keep
// its turn open until the returned channel closes.
func (c *chief) hold(id string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held[id] = make(chan struct{})
	return c.held[id]
}

func (c *chief) turn(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
	c.mu.Lock()
	released, held := maps.Clone(c.released), maps.Clone(c.held)
	c.mu.Unlock()
	for _, m := range openQuestion.FindAllStringSubmatch(req.Prompt, -1) {
		id := m[1]
		switch {
		case released[id] || held[id] != nil:
			c.choose(ctx, tools, questions.AnswerTool, map[string]any{"question": id, "text": askedAnswer, "citations": []string{"charter#1"}})
		default:
			c.choose(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{id}, "rephrasing": "Is the store durable across restarts?", "blocked": "The committee's round.", "options": []string{"Yes", "No"}, "recommendation": "Yes."})
		}
		if hold := held[id]; hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	for _, m := range ownerRuled.FindAllStringSubmatch(req.Prompt, -1) {
		c.choose(ctx, tools, questions.RelayRulingTool, map[string]any{"question": strings.Split(m[1], ", ")[0], "text": relayedRuling, "scope": "local"})
	}
	return questionResult(req, "session-chief", "Chosen"), nil
}

// choose calls one question tool and reports a refusal, except one saying the
// question was chosen for already: a prompt replays earlier events.
func (c *chief) choose(ctx context.Context, tools *mcp.ClientSession, name string, args map[string]any) {
	got, err := callTool(ctx, tools, name, args)
	if err != nil {
		c.p.report("%s: %v", name, err)
		return
	}
	var result struct {
		Recorded bool   `json:"recorded"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(got), &result); err != nil {
		c.p.report("%s returned %q", name, got)
		return
	}
	if !result.Recorded && !strings.Contains(result.Reason, "is already answered") && !strings.Contains(result.Reason, "is escalated to the owner") && !strings.Contains(result.Reason, "has the owner's ruling") {
		c.p.report("%s refused: %s", name, result.Reason)
	}
}

// newAskingFixture is a debate fixture whose service also runs the chief of
// staff through the production thread reconciler, so a member's question is
// chosen for, ruled on and delivered.
func newAskingFixture(t *testing.T, members, rounds int, p *faults) (*shedFixture, *chief) {
	t.Helper()
	f := newDebateFixtureWith(t, members, rounds, chiefRole, func(opts *Options) {
		opts.Threads = Enforce(*opts, Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts}).Threads
	})
	c := &chief{p: p, released: map[string]bool{}, held: map[string]chan struct{}{}}
	f.engine.mu.Lock()
	defer f.engine.mu.Unlock()
	f.engine.turns["*"] = c.turn
	return f, c
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

// asks makes a member's turn ask the question that gets number id and check
// the tool's answer.
func asks(p *faults, id string) fakeTurn {
	return func(ctx context.Context, _ agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		got, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": askedQuestion})
		if err != nil || got != `{"recorded":true,"question":"`+id+`","next":"End your turn now. The answer arrives as your next turn on this thread."}` {
			p.report("ask: %q %v", got, err)
		}
		if got, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "And another?"}); err != nil || !strings.Contains(got, "this turn already asked question "+id) {
			p.report("a second question in the turn: %q %v", got, err)
		}
		return nil
	}
}

// rule has the owner rule on the inbox entry that escalated question id, once
// it is there.
func (f *shedFixture) rule(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(demoTimeout)
	for {
		inbox, err := f.c.Inbox(ctx)
		must(t, err)
		for _, e := range inbox.Entries {
			if slices.ContainsFunc(e.Asked, func(q InboxQuestion) bool { return q.ID == id }) {
				if _, err := f.c.Answer(ctx, e.Number, ownerRuling); err != nil {
					t.Fatalf("rule on inbox entry %d: %v", e.Number, err)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("question %s was never escalated: %+v", id, inbox.Entries)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f *shedFixture) thread(t *testing.T, stream config.WorkstreamID, member string) trace.Thread {
	t.Helper()
	th, err := f.repository().Thread(stream, member)
	must(t, err)
	return th
}

func (f *shedFixture) question(t *testing.T, stream config.WorkstreamID, id string) trace.QuestionState {
	t.Helper()
	asked, err := f.repository().Questions(stream)
	must(t, err)
	for _, q := range asked {
		if q.Asked.ID == id {
			return q
		}
	}
	t.Fatalf("no question %s: %+v", id, asked)
	return trace.QuestionState{}
}

// awaitQuestion waits until question id is in the given state.
func (f *shedFixture) awaitQuestion(t *testing.T, stream config.WorkstreamID, id, state string) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for f.question(t, stream, id).State != state {
		if time.Now().After(deadline) {
			t.Fatalf("question %s never became %s: %+v", id, state, f.question(t, stream, id))
		}
		time.Sleep(50 * time.Millisecond)
	}
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

// roundOperation returns the acknowledged round operation with the given
// resumption number.
func (f *shedFixture) roundOperation(t *testing.T, stream config.WorkstreamID, resume int) trace.OperationRecord {
	t.Helper()
	ops := f.acknowledgedRoundOperations(t, stream)
	for _, o := range ops {
		if in, err := decodeRound(o.Operation); err == nil && in.Resume == resume {
			return o
		}
	}
	t.Fatalf("no round operation with resumption %d: %v", resume, results(ops))
	return trace.OperationRecord{}
}

func (f *shedFixture) record(t *testing.T, stream config.WorkstreamID, member string) shed.Record {
	t.Helper()
	records, err := shed.Records(f.repository(), stream)
	must(t, err)
	for _, r := range records {
		if r.Member == member {
			return r
		}
	}
	t.Fatalf("no record of %s: %+v", member, records)
	return shed.Record{}
}

// A member that asks parks the round: its turn ends waiting, the round
// operation ends waiting, the question is in the chief of staff's hands, no
// reply is requested and the workstream stays in the shed, across a restart
// and through the owner's actions. The owner's ruling, relayed by the chief of
// staff, runs as the member's next turn against the same pinned revision,
// continuing what it contributed before asking, and the round is heard with
// every member.
func TestMemberQuestionParksTheRoundUntilTheAnswerArrives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &faults{}
	f, _ := newAskingFixture(t, 2, 1, p)
	defer f.stop(t)
	member := committeeAgent(1)
	first, second := shed.ObjectionID(1, member, 1), shed.ObjectionID(1, member, 2)
	asking := f.member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(objects(p, shed.Fit, "plan", "spec#1")(ctx, req, verified, tools), asks(p, "1")(ctx, req, verified, tools))
	})
	other := f.member(1, 2, 1, silent)
	answering := f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, want := range []string{"The owner ruled on your question 1. The chief of staff relays the ruling.", "| " + askedQuestion, "<<< osmia:owner_response", "| " + relayedRuling} {
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
		return concedes(p, first, second)(ctx, req, nil, tools)
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
	if q := f.question(t, stream, "1"); q.Asked.AskedBy.ID != member || q.Asked.Turn != asking || q.Asked.Question != askedQuestion {
		t.Fatalf("question: %+v", q)
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

	// The question reaches the owner's inbox through the chief of staff, and
	// a restart keeps the round parked: the pass after it requests nothing.
	f.awaitQuestion(t, stream, "1", trace.QuestionEscalated)
	f.stop(t)
	f.start(t)
	if state, err := f.repository().Workflow(stream, shedSubject); err != nil || state.Value != "waiting-1" {
		t.Fatalf("shed after the restart: %+v %v", state, err)
	}
	must(t, (&debate{s: f.s, repository: f.repository()}).Pass(ctx))
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "waiting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v after the restart, want %v", moves, want)
	}
	inbox, err := f.c.Inbox(ctx)
	if err != nil || len(inbox.Entries) != 1 || inbox.Entries[0].Workstream != stream || !slices.Equal(inbox.Entries[0].Asked, []InboxQuestion{{ID: "1", AskedBy: member, Question: askedQuestion}}) {
		t.Fatalf("inbox: %+v %v", inbox, err)
	}

	// The owner rules; the relayed ruling runs as the member's next turn and
	// the round is heard with both members.
	f.rule(t, "1")
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	// The owner's objection is the one that stands in the record; the ruling
	// that dismissed it is read at the conclusion.
	heard := fmt.Sprintf("round 1 against %s: 2 members heard, 2 objections, 2 concessions, 0 failed turns; 1 objections stand", pinOne)
	resumed := f.roundOperation(t, stream, 1)
	if resumed.Result.Outcome != "succeeded" || resumed.Result.Evidence != heard || len(f.acknowledgedRoundOperations(t, stream)) != 2 {
		t.Fatalf("round operations: %v", results(f.roundOperations(t, stream)))
	}
	if in, err := decodeRound(resumed.Operation); err != nil || in != (roundInput{Round: 1, Spec: 1, Plan: 1, Resume: 1}) {
		t.Fatalf("resumption input %+v %v", in, err)
	}
	resumption := f.transition(t, stream, "shed-round-1-resume-1")
	if resumption.From != "waiting-1" || resumption.To != "round-1" || resumption.Cause != "shed-round-1-waiting-1" || resumption.Actor != shedActor || resumption.Reason != fmt.Sprintf("round 1 against %s resumes: every member that asked has its answer", pinOne) {
		t.Fatalf("resumption: %+v", resumption)
	}
	if told := f.transition(t, stream, "shed-round-1-heard"); told.Cause != resumed.Operation.ID || told.Reason != heard {
		t.Fatalf("heard: %+v", told)
	}
	mine := f.record(t, stream, member)
	if mine.Turn != asking || mine.Failure != "" || len(mine.Objections) != 2 || mine.Objections[1].ID != second ||
		!slices.Equal(mine.Concessions, []shed.Concession{{Objection: first, Reason: "The reply settles it."}, {Objection: second, Reason: "The reply settles it."}}) {
		t.Fatalf("record: %+v", mine)
	}
	if end := f.transition(t, stream, "shed-concluded-1"); end.Reason != "debate concluded after round 1: the owner disposed of every objection that stood" {
		t.Fatalf("conclusion: %+v", end)
	}
	if ran := f.ran(); ran[asking] != 1 || ran[other] != 1 || ran[answering] != 1 || ran[roundTurnID(1, member, 2)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	if q := f.question(t, stream, "1"); q.State != trace.QuestionAnswered || q.Ruling == nil || q.Ruling.Decision != trace.DecisionRuling || q.Ruling.ReturnedAnswer != relayedRuling {
		t.Fatalf("question: %+v", q)
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
	f, _ := newAskingFixture(t, 1, 1, p)
	defer f.stop(t)
	member := committeeAgent(1)
	f.member(1, 1, 1, asks(p, "1"))
	answering := f.answer("1", silent)
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "waiting-1", "heard-1", "failed-1")
	p.check(t)
	if skipped, err := f.c.ShedSkip(ctx, stream); err != nil || skipped.Action != skippedValue {
		t.Fatalf("skip while parked: %+v %v", skipped, err)
	}
	f.rule(t, "1")
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
	if skipped, err := skippedDebate(f.repository(), stream); err != nil || !skipped {
		t.Fatalf("the skip is not recorded: %v %v", skipped, err)
	}
	f.stillInShed(t, stream)
}

// A service stop that interrupts the answer's turn is recovered like any
// other: the member's next attempt carries the answer it received, and the
// round is heard with what that attempt contributed.
func TestInterruptedAnswerTurnIsRetriedWithTheAnswer(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.stop(t)
	c.release("1")
	member := committeeAgent(1)
	asking := f.member(1, 1, 1, asks(p, "1"))
	started := make(chan struct{})
	var once sync.Once
	answering := f.answer("1", func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	})
	retried := f.member(1, 1, 2, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		for _, want := range []string{"Round 1 of the shed", "The answers to the questions you asked in this round:", "Answer to your question 1.", "| " + askedQuestion, "<<< osmia:answer", "| " + askedAnswer} {
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
	if mine := f.record(t, stream, member); mine.Turn != retried || !mine.Silent() {
		t.Fatalf("record: %+v", mine)
	}
	if end := f.transition(t, stream, "shed-concluded-1"); end.Reason != "debate concluded by consensus after round 1: no objection stands" {
		t.Fatalf("conclusion: %+v", end)
	}
}

// A turn that asked and was then interrupted by a service stop parks the
// round like one that ended waiting: the member gets no new attempt, the
// answer runs on its thread, and the round is heard with what the interrupted
// turn contributed before asking.
func TestAskingTurnInterruptedByAStopStillParksTheRound(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.stop(t)
	c.release("1")
	member := committeeAgent(1)
	first := shed.ObjectionID(1, member, 1)
	asked := make(chan struct{})
	asking := f.member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if err := errors.Join(objects(p, shed.Fit, "plan", "spec#1")(ctx, req, verified, tools), asks(p, "1")(ctx, req, verified, tools)); err != nil {
			return err
		}
		close(asked)
		<-ctx.Done()
		return ctx.Err()
	})
	answering := f.answer("1", concedes(p, first))
	stream := f.handIn(t, "design", handedDesign)
	select {
	case <-asked:
	case <-time.After(demoTimeout):
		t.Fatal("the member never asked")
	}
	f.stop(t)
	f.start(t)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if park := f.transition(t, stream, "shed-round-1-waiting-1"); park.Reason != fmt.Sprintf("round 1 against %s is parked: %s waits for the answer to question 1", pinOne, member) {
		t.Fatalf("park: %+v", park)
	}
	if ran := f.ran(); ran[asking] != 1 || ran[answering] != 1 || ran[roundTurnID(1, member, 2)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	th := f.thread(t, stream, member)
	if len(th.Turns) != 2 || th.Turns[0].Status() != "interrupted" || th.Turns[1].Request.TurnID != answering || th.Turns[1].Status() != "idle" {
		t.Fatalf("asker's thread: %+v", th)
	}
	if mine := f.record(t, stream, member); mine.Turn != asking || mine.Failure != "" || len(mine.Objections) != 1 || len(mine.Concessions) != 1 {
		t.Fatalf("record: %+v", mine)
	}
	if end := f.transition(t, stream, "shed-concluded-1"); end.Reason != "debate concluded by consensus after round 1: no objection stands" {
		t.Fatalf("conclusion: %+v", end)
	}
}

// An answer queued behind a turn a crashed service left reserved runs once
// that turn is abandoned: the round is heard with the answer, not held behind
// the reservation. The reservation is planted between lifetimes: a stop
// completes the turn it interrupts, a crash does not.
func TestAnswerQueuedBehindAReservedTurnRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &faults{}
	f, _ := newAskingFixture(t, 1, 1, p)
	defer f.stop(t)
	member := committeeAgent(1)
	asked := make(chan struct{})
	asking := f.member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if err := asks(p, "1")(ctx, req, verified, tools); err != nil {
			return err
		}
		close(asked)
		<-ctx.Done()
		return ctx.Err()
	})
	answering := f.answer("1", silent)
	stream := f.handIn(t, "design", handedDesign)
	select {
	case <-asked:
	case <-time.After(demoTimeout):
		t.Fatal("the member never asked")
	}
	// The chief of staff's turns run beside the round; each has been claimed
	// before the service stops.
	eventually(t, "the chief of staff's turns were never claimed", func() bool {
		th, err := f.repository().Thread(stream, trace.ChiefOfStaff)
		must(t, err)
		return !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Claim == nil && q.CompletedAt.IsZero() })
	})
	f.stop(t)
	// Between lifetimes a second attempt is reserved on the member's thread
	// as a crash would leave it, the chief of staff answers, and the answer
	// is queued behind the reservation.
	cfg, err := config.Load(f.opts.Config)
	must(t, err)
	repo, err := trace.Open(cfg.Root, config.Project{ID: f.project, Clone: f.clone})
	must(t, err)
	th, err := repo.Thread(stream, member)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Status() != "interrupted" {
		t.Fatalf("asking turn after the stop: %+v", th.Turns)
	}
	reserved := roundTurnID(1, member, 2)
	second := th.Turns[0].Request
	second.ID, second.TurnID, second.At = "request_"+reserved, reserved, f.clock.Now()
	_, err = repo.EnqueueTurn(ctx, second)
	must(t, err)
	home := filepath.Dir(f.clone)
	_, err = repo.ClaimTurn(ctx, stream, member, "token_"+reserved, filepath.Join(home, reserved), f.clock.Now())
	must(t, err)
	chiefHeader := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_planted", Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "planted"}
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: chiefHeader, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "planted", Profile: second.Profile, Prompt: "Answer"})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token_planted", filepath.Join(home, "planted"), f.clock.Now())
	must(t, err)
	chiefScope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Thread: trace.ChiefOfStaff, Turn: "planted", Role: trace.ChiefOfStaff}
	_, err = repo.AnswerQuestion(ctx, trace.ChiefOfStaff, chiefScope, "1", askedAnswer, []string{"charter#1"}, f.clock.Now())
	must(t, err)
	// The chief of staff's planted turn is abandoned as a crashed one would be:
	// by the next session to open the trace.
	must(t, repo.Close())
	repo, err = trace.Open(cfg.Root, config.Project{ID: f.project, Clone: f.clone})
	must(t, err)
	must(t, repo.AbandonTurn(ctx, stream, trace.ChiefOfStaff, "planted", f.clock.Now()))
	planted, err := repo.Questions(stream)
	must(t, err)
	if len(planted) != 1 || planted[0].State != trace.QuestionAnswered {
		t.Fatalf("planted questions: %+v", planted)
	}
	if delivered, err := questions.Deliver(ctx, repo, planted[0], func(string) (coreadapter.Profile, error) {
		return coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, nil
	}, f.clock.Now()); err != nil || !delivered {
		t.Fatalf("deliver: %t %v", delivered, err)
	}
	if th, err := repo.Thread(stream, member); err != nil || len(th.Turns) != 3 || th.Turns[1].Claim == nil || th.Turns[1].Response != nil || th.Turns[2].Request.TurnID != answering {
		t.Fatalf("planted thread: %+v %v", th, err)
	}
	must(t, repo.Close())
	f.start(t)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	th = f.thread(t, stream, member)
	if len(th.Turns) != 3 || th.Turns[1].Request.TurnID != reserved || th.Turns[1].Status() != "interrupted" || th.Turns[2].Request.TurnID != answering || th.Turns[2].Status() != "idle" {
		t.Fatalf("asker's thread: %+v", th)
	}
	if ran := f.ran(); ran[asking] != 1 || ran[answering] != 1 || ran[reserved] != 0 || ran[roundTurnID(1, member, 3)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	if mine := f.record(t, stream, member); mine.Turn != asking || !mine.Silent() {
		t.Fatalf("record: %+v", mine)
	}
}

// A turn that asked and then failed parks the round too: the answer runs on
// the member's thread, and the record is what the chain ended with.
func TestTurnThatFailedAfterAskingParksTheRound(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.stop(t)
	c.release("1")
	member := committeeAgent(1)
	asking := f.member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if err := asks(p, "1")(ctx, req, verified, tools); err != nil {
			return err
		}
		return errors.New("the backend crashed after the question")
	})
	answering := f.answer("1", silent)
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	th := f.thread(t, stream, member)
	if len(th.Turns) != 2 || th.Turns[0].Status() != "failed" || th.Turns[1].Request.TurnID != answering || th.Turns[1].Status() != "idle" {
		t.Fatalf("asker's thread: %+v", th)
	}
	if ran := f.ran(); ran[asking] != 1 || ran[answering] != 1 || ran[roundTurnID(1, member, 2)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	if mine := f.record(t, stream, member); mine.Turn != asking || !mine.Silent() {
		t.Fatalf("record: %+v", mine)
	}
	if told := f.transition(t, stream, "shed-round-1-heard"); told.Reason != fmt.Sprintf("round 1 against %s: 1 members heard, 0 objections, 0 concessions, 0 failed turns; 0 objections stand", pinOne) {
		t.Fatalf("heard: %+v", told)
	}
}

// A member that asks again in its answer turn parks the round again: the
// second park is caused by the resumed operation, the second resumption is
// numbered, and every turn of the chain contributes to the one record.
func TestMemberAsksAgainInItsAnswerTurn(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.stop(t)
	c.release("1")
	c.release("2")
	member := committeeAgent(1)
	ids := []string{shed.ObjectionID(1, member, 1), shed.ObjectionID(1, member, 2), shed.ObjectionID(1, member, 3)}
	asking := f.member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(objects(p, shed.Fit, "plan", "spec#1")(ctx, req, verified, tools), asks(p, "1")(ctx, req, verified, tools))
	})
	f.answer("1", func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if recorded, id, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "proof", "part": "spec#2", "argument": "No unit shows it.", "citations": []string{"plan#dedupe"}}); err != nil || !recorded || id != ids[1] {
			p.report("objection in the first answer turn: %v %q %v", recorded, id, err)
		}
		return asks(p, "2")(ctx, req, verified, tools)
	})
	f.answer("2", func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, "Answer to your question 2.") {
			p.report("second answer prompt:\n%s", req.Prompt)
		}
		if recorded, id, err := shedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "size", "part": "plan#resume", "argument": "Too wide.", "citations": []string{"kb/entities.json#internal.trace"}}); err != nil || !recorded || id != ids[2] {
			p.report("objection in the second answer turn: %v %q %v", recorded, id, err)
		}
		return concedes(p, ids...)(ctx, req, verified, tools)
	})
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	// The state names the round both times; the transitions number the parks.
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	first, second := f.roundOperation(t, stream, 1), f.roundOperation(t, stream, 2)
	parked := fmt.Sprintf("round 1 against %s is parked: %s waits for the answer to question 2", pinOne, member)
	if first.Result.Outcome != "waiting" || first.Result.Evidence != parked || second.Result.Outcome != "succeeded" {
		t.Fatalf("round operations: %v", results(f.roundOperations(t, stream)))
	}
	if park := f.transition(t, stream, "shed-round-1-waiting-2"); park.Cause != first.Operation.ID || park.From != "round-1" || park.To != "waiting-1" || park.Reason != parked {
		t.Fatalf("second park: %+v", park)
	}
	if resumption := f.transition(t, stream, "shed-round-1-resume-2"); resumption.Cause != "shed-round-1-waiting-2" || resumption.From != "waiting-1" || resumption.To != "round-1" {
		t.Fatalf("second resumption: %+v", resumption)
	}
	if in, err := decodeRound(second.Operation); err != nil || in != (roundInput{Round: 1, Spec: 1, Plan: 1, Resume: 2}) {
		t.Fatalf("second resumption input %+v %v", in, err)
	}
	if told := f.transition(t, stream, "shed-round-1-heard"); told.Cause != second.Operation.ID {
		t.Fatalf("heard: %+v", told)
	}
	mine := f.record(t, stream, member)
	if mine.Turn != asking || len(mine.Objections) != 3 || len(mine.Concessions) != 3 || mine.Failure != "" {
		t.Fatalf("record: %+v", mine)
	}
	for i, o := range mine.Objections {
		if o.ID != ids[i] {
			t.Fatalf("objection %d is %s", i+1, o.ID)
		}
	}
	if th := f.thread(t, stream, member); len(th.Turns) != 3 || th.Turns[0].Status() != "waiting" || th.Turns[1].Status() != "waiting" || th.Turns[2].Status() != "idle" {
		t.Fatalf("asker's thread: %+v", th)
	}
}

// Two members ask in one round: the park names both, and the round resumes
// only once the second answer is queued, not when the first is.
func TestRoundResumesOnlyWhenEveryAskerHasItsAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &faults{}
	f, c := newAskingFixture(t, 2, 1, p)
	defer f.stop(t)
	c.release("1")
	one, two := committeeAgent(1), committeeAgent(2)
	firstAsked := make(chan struct{})
	f.member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		defer close(firstAsked)
		return asks(p, "1")(ctx, req, verified, tools)
	})
	f.member(1, 2, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		select {
		case <-firstAsked:
		case <-ctx.Done():
			return ctx.Err()
		}
		return asks(p, "2")(ctx, req, verified, tools)
	})
	answerOne, answerTwo := f.answer("1", silent), f.answer("2", silent)
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "waiting-1", "heard-1", "failed-1")
	p.check(t)
	parked := fmt.Sprintf("round 1 against %s is parked: %s waits for the answer to question 1; %s waits for the answer to question 2", pinOne, one, two)
	if park := f.transition(t, stream, "shed-round-1-waiting-1"); park.Reason != parked {
		t.Fatalf("park: %+v", park)
	}
	// The first answer is queued while the second question is with the
	// owner: a pass requests nothing.
	queued := f.awaitTurn(t, stream, one, answerOne)
	f.awaitQuestion(t, stream, "2", trace.QuestionEscalated)
	must(t, (&debate{s: f.s, repository: f.repository()}).Pass(ctx))
	if state, err := f.repository().Workflow(stream, shedSubject); err != nil || state.Value != "waiting-1" || !queued.CompletedAt.IsZero() || len(f.roundOperations(t, stream)) != 1 {
		t.Fatalf("shed with one answer queued: %+v %v", state, err)
	}
	if ran := f.ran(); ran[answerOne] != 0 {
		t.Fatalf("the first answer ran before the second arrived: %v", ran)
	}
	f.rule(t, "2")
	f.awaitShed(t, stream, "concluded-1")
	p.check(t)
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if ran := f.ran(); ran[answerOne] != 1 || ran[answerTwo] != 1 {
		t.Fatalf("turns ran: %v", ran)
	}
	if told := f.transition(t, stream, "shed-round-1-heard"); told.Reason != fmt.Sprintf("round 1 against %s: 2 members heard, 0 objections, 0 concessions, 0 failed turns; 0 objections stand", pinOne) {
		t.Fatalf("heard: %+v", told)
	}
	for _, member := range []string{one, two} {
		if mine := f.record(t, stream, member); !mine.Silent() {
			t.Fatalf("record of %s: %+v", member, mine)
		}
	}
}

// Abandoning the workstream while a round is parked leaves it parked: the
// answer, recorded before the abandonment, is never delivered, no turn runs,
// and the round neither records nor fails.
func TestAbandoningAParkedRoundLeavesItParked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.stop(t)
	hold := c.hold("1")
	member := committeeAgent(1)
	f.member(1, 1, 1, asks(p, "1"))
	answering := f.answer("1", silent)
	stream := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, stream, "waiting-1", "heard-1", "failed-1")
	// The answer is recorded, and its delivery waits for the chief of
	// staff's turn to end.
	f.awaitQuestion(t, stream, "1", trace.QuestionAnswered)
	if _, err := f.c.Abandon(ctx, stream, "Superseded."); err != nil {
		t.Fatal(err)
	}
	close(hold)
	must(t, f.s.answers(f.s.current(), f.repository()).Pass(ctx))
	must(t, (&debate{s: f.s, repository: f.repository()}).Pass(ctx))
	if moves, want := f.shedMoves(t, stream), []string{"round-1", "waiting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if th := f.thread(t, stream, member); len(th.Turns) != 1 || th.Turns[0].Status() != "waiting" {
		t.Fatalf("asker's thread after the abandonment: %+v", th)
	}
	if ran := f.ran(); ran[answering] != 0 {
		t.Fatalf("the answer ran: %v", ran)
	}
	if records, err := shed.Records(f.repository(), stream); err != nil || len(records) != 0 {
		t.Fatalf("records: %+v %v", records, err)
	}
	if feature, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AbandonedState {
		t.Fatalf("feature: %+v %v", feature, err)
	}
	if q := f.question(t, stream, "1"); q.State != trace.QuestionAnswered {
		t.Fatalf("question: %+v", q)
	}
}
