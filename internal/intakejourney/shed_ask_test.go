package intakejourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	sync "sync"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	plan "github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/projectstore"
	questions "github.com/kpenfound/osmia/internal/questions"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	thread "github.com/kpenfound/osmia/internal/thread"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newAskingFixture(t *testing.T, members, rounds int, p *intakefixture.Faults) (*journey.ShedFixture, *intakefixture.Manager) {
	t.Helper()
	return newAskingFixtureWith(t, members, rounds, p, nil)
}

func newAskingFixtureWith(t *testing.T, members, rounds int, p *intakefixture.Faults, prepare func(*journey.Options)) (*journey.ShedFixture, *intakefixture.Manager) {
	t.Helper()
	f := journey.NewDebateFixtureWith(t, members, rounds, intakefixture.ManagerRole, func(opts *journey.Options) {
		opts.Threads = journey.Enforce(*opts, service.Enforcement{Engine: opts.Committee.Engine, Hosts: opts.Committee.Hosts}).Threads
		if prepare != nil {
			prepare(opts)
		}
	})
	c := &intakefixture.Manager{P: p, Released: map[string]bool{}, Held: map[string]chan struct{}{}}
	f.Engine.Mu.Lock()
	defer f.Engine.Mu.Unlock()
	f.Engine.Turns["*"] = c.Turn
	return f, c
}

func awaitDocument(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID, id string) {
	t.Helper()
	f.S.Until(t, "document "+id+" to be recorded", func() bool { return len(f.Documents(t, stream, id)) > 0 })
}

func TestMemberQuestionParksTheRoundUntilTheAnswerArrives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	f, _ := newAskingFixture(t, 2, 1, p)
	defer f.Stop(t)
	member := intake.CommitteeAgent(1)
	first, second := shed.ObjectionID(1, member, 1), shed.ObjectionID(1, member, 2)
	asking := f.Member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(intakefixture.Objects(p, shed.Fit, "plan", "spec#1")(ctx, req, verified, tools), intakefixture.Asks(p, "1")(ctx, req, verified, tools))
	})
	other := f.Member(1, 2, 1, intakefixture.Silent)
	answering := f.Answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		for _, want := range []string{"The owner ruled on your question 1. The chief of staff relays the ruling.", "| " + intakefixture.AskedQuestion, "<<< osmia:owner_response", "| " + intakefixture.RelayedRuling} {
			if !strings.Contains(req.Prompt, want) {
				p.Report("answer prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		if !strings.Contains(req.SystemPrompt, "member of the committee") || req.Profile.Name != intake.CommitteeRole {
			p.Report("answer turn system prompt %q, profile %+v", req.SystemPrompt, req.Profile)
		}
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil || !slices.Equal(names, []string{questions.AskTool, shed.ConcedeTool, "file_read", shed.ObjectTool, "project_context"}) {
			p.Report("answer turn tools %v %v", names, err)
		}

		if got, err := intakefixture.ReadTool(ctx, tools, "spec.md"); err != nil || got != fixturekit.ValidSpec {
			p.Report("the answer turn read spec %q %v", got, err)
		}

		if recorded, id, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "acceptance", "part": "plan#dedupe", "argument": "No unit shows it.", "citations": []string{"spec#2"}}); err != nil || !recorded || id != second {
			p.Report("objection after the answer: %v %q %v", recorded, id, err)
		}
		return intakefixture.Concedes(p, first, second)(ctx, req, nil, tools)
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "waiting-1", "heard-1", "failed-1")
	p.Check(t)

	ops := f.AcknowledgedRoundOperations(t, stream)
	parked := fmt.Sprintf("round 1 against %s is parked: %s waits for the answer to question 1", intakefixture.PinOne, member)
	if len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "waiting" || ops[0].Result.Evidence != parked {
		t.Fatalf("round operations: %v", intakefixture.Results(ops))
	}
	park := f.Transition(t, stream, "shed-round-1-waiting-1")
	if park.From != "round-1" || park.To != "waiting-1" || park.Cause != ops[0].Operation.ID || park.Reason != parked || park.Actor != intake.ShedActor {
		t.Fatalf("park: %+v", park)
	}

	asker := f.Thread(t, stream, member)
	if last := asker.Turns[len(asker.Turns)-1]; !asker.Parked() || last.Request.TurnID != asking || last.Status() != "waiting" || last.Response.Result.Outcome == nil || *last.Response.Result.Outcome != (coreadapter.Outcome{Status: "waiting", Report: "Asked question 1"}) {
		t.Fatalf("asker's thread: %+v", asker)
	}
	if th := f.Thread(t, stream, intake.CommitteeAgent(2)); th.Parked() || th.Turns[len(th.Turns)-1].Status() != "idle" {
		t.Fatalf("other member's thread: %+v", th)
	}
	if q := f.Question(t, stream, "1"); q.Asked.AskedBy.ID != member || q.Asked.Turn != asking || q.Asked.Question != intakefixture.AskedQuestion {
		t.Fatalf("question: %+v", q)
	}
	if records, err := shed.Records(f.Repository(), stream); err != nil || len(records) != 0 {
		t.Fatalf("records of a parked round: %+v %v", records, err)
	}
	if len(f.ReplyOperations(t, stream)) != 0 {
		t.Fatal("the architect was asked for a reply while the round is parked")
	}
	f.StillInShed(t, stream)

	objected, err := f.C.ShedObject(ctx, stream, "The plan never names the retry budget.")
	if err != nil || objected.Action != "objected" || objected.Round != 1 {
		t.Fatalf("owner objection while parked: %+v %v", objected, err)
	}
	if ruled, err := f.C.ShedRule(ctx, stream, objected.Objection, "dismiss", "The budget is the default."); err != nil || ruled.Action != "dismissed" {
		t.Fatalf("owner ruling while parked: %+v %v", ruled, err)
	}
	edited := strings.Replace(fixturekit.ValidSpec, "never sent again", "never sent twice", 1)
	servicetest.EditDraft(t, f.C.Client, stream, edited, "")

	f.AwaitQuestion(t, stream, "1", trace.QuestionEscalated)
	f.Stop(t)
	f.Start(t)
	if state, err := f.Repository().Workflow(stream, intake.ShedSubject); err != nil || state.Value != "waiting-1" {
		t.Fatalf("shed after the restart: %+v %v", state, err)
	}
	fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).Pass(ctx))
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "waiting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v after the restart, want %v", moves, want)
	}
	inbox, err := f.C.Inbox(ctx)
	if err != nil || len(inbox.Entries) != 1 || inbox.Entries[0].Workstream != stream || !slices.Equal(inbox.Entries[0].Asked, []api.InboxQuestion{{ID: "1", AskedBy: member, Question: intakefixture.AskedQuestion}}) {
		t.Fatalf("inbox: %+v %v", inbox, err)
	}

	f.Rule(t, "1")
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}

	heard := fmt.Sprintf("round 1 against %s: 2 members heard, 2 objections, 2 concessions, 0 failed turns; 1 objections stand", intakefixture.PinOne)
	resumed := f.RoundOperation(t, stream, 1)
	if resumed.Result.Outcome != "succeeded" || resumed.Result.Evidence != heard || len(f.AcknowledgedRoundOperations(t, stream)) != 2 {
		t.Fatalf("round operations: %v", intakefixture.Results(f.RoundOperations(t, stream)))
	}
	if in, err := intake.DecodeRound(resumed.Operation); err != nil || in != (intake.RoundInput{Round: 1, Spec: 1, Plan: 1, Resume: 1}) {
		t.Fatalf("resumption input %+v %v", in, err)
	}
	resumption := f.Transition(t, stream, "shed-round-1-resume-1")
	if resumption.From != "waiting-1" || resumption.To != "round-1" || resumption.Cause != "shed-round-1-waiting-1" || resumption.Actor != intake.ShedActor || resumption.Reason != fmt.Sprintf("round 1 against %s resumes: every member that asked has its answer", intakefixture.PinOne) {
		t.Fatalf("resumption: %+v", resumption)
	}
	if told := f.Transition(t, stream, "shed-round-1-heard"); told.Cause != resumed.Operation.ID || told.Reason != heard {
		t.Fatalf("heard: %+v", told)
	}
	mine := f.Record(t, stream, member)
	if mine.Turn != asking || mine.Failure != "" || len(mine.Objections) != 2 || mine.Objections[1].ID != second ||
		!slices.Equal(mine.Concessions, []shed.Concession{{Objection: first, Reason: "The reply settles it."}, {Objection: second, Reason: "The reply settles it."}}) {
		t.Fatalf("record: %+v", mine)
	}
	if end := f.Transition(t, stream, "shed-concluded-1"); end.Reason != "debate concluded after round 1: the owner disposed of every objection that stood" {
		t.Fatalf("conclusion: %+v", end)
	}
	if ran := f.Ran(); ran[asking] != 1 || ran[other] != 1 || ran[answering] != 1 || ran[intake.RoundTurnID(1, member, 2)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	if q := f.Question(t, stream, "1"); q.State != trace.QuestionAnswered || q.Ruling == nil || q.Ruling.Decision != trace.DecisionRuling || q.Ruling.ReturnedAnswer != intakefixture.RelayedRuling {
		t.Fatalf("question: %+v", q)
	}
	if th := f.Thread(t, stream, member); th.Parked() || len(th.Turns) != 2 || th.Turns[1].Request.TurnID != answering || th.Turns[1].Status() != "idle" || th.Turns[1].Request.SystemPrompt != th.Turns[0].Request.SystemPrompt {
		t.Fatalf("asker's thread: %+v", th)
	}
	if specs := f.Documents(t, stream, plan.SpecDocument); len(specs) != 2 || specs[1].Content != edited {
		t.Fatalf("the owner's edit while parked was not recorded: %+v", specs)
	}
	f.StillInShed(t, stream)

	f.Await(t, stream, func(_, _ string) bool {
		threads, err := f.Repository().Threads(stream)
		fixturekit.Must(t, err)
		for _, th := range threads {
			for _, q := range th.Turns {
				if q.CompletedAt.IsZero() {
					return false
				}
			}
		}
		return true
	})
	root := f.Opts.Config.Root
	if _, err := os.Lstat(filepath.Join(root, "shed", string(f.Project), string(stream), answering, "workspace")); !os.IsNotExist(err) {
		t.Fatal("the answer turn's staged workspace is retained")
	}
	if entries, err := os.ReadDir(filepath.Join(root, "views")); err != nil || len(entries) != 0 {
		t.Fatalf("views leaked: %v %v", entries, err)
	}
}

func TestOwnerSkipsDebateWhileARoundIsParked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	f, _ := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	member := intake.CommitteeAgent(1)
	f.Member(1, 1, 1, intakefixture.Asks(p, "1"))
	answering := f.Answer("1", intakefixture.Silent)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "waiting-1", "heard-1", "failed-1")
	p.Check(t)
	if skipped, err := f.C.ShedSkip(ctx, stream); err != nil || skipped.Action != intake.SkippedValue {
		t.Fatalf("skip while parked: %+v %v", skipped, err)
	}
	f.Rule(t, "1")

	queued := f.AwaitTurn(t, stream, member, answering)
	awaitDocument(t, f, stream, shed.PacketDocumentID(1))
	fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).Pass(ctx))
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "waiting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if ran := f.Ran(); ran[answering] != 0 || !queued.CompletedAt.IsZero() || len(f.RoundOperations(t, stream)) != 1 {
		t.Fatalf("a skipped debate ran the answer: %v", ran)
	}
	if skipped, err := intake.SkippedDebate(f.Repository(), stream); err != nil || !skipped {
		t.Fatalf("the skip is not recorded: %v %v", skipped, err)
	}
	f.StillInShed(t, stream)
}

func TestInterruptedAnswerTurnIsRetriedWithTheAnswer(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	c.Release("1")
	member := intake.CommitteeAgent(1)
	asking := f.Member(1, 1, 1, intakefixture.Asks(p, "1"))
	started := make(chan struct{})
	var once sync.Once
	answering := f.Answer("1", func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	})
	retried := f.Member(1, 1, 2, func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		for _, want := range []string{"Round 1 of the shed", "The answers to the questions you asked in this round:", "Answer to your question 1.", "| " + intakefixture.AskedQuestion, "<<< osmia:answer", "| " + intakefixture.AskedAnswer} {
			if !strings.Contains(req.Prompt, want) {
				p.Report("retried attempt's prompt lacks %q:\n%s", want, req.Prompt)
			}
		}
		return nil
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	select {
	case <-started:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the answer's turn never started")
	}
	f.Stop(t)
	f.Start(t)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if ran := f.Ran(); ran[asking] != 1 || ran[answering] != 1 || ran[retried] != 1 {
		t.Fatalf("turns ran: %v", ran)
	}
	th := f.Thread(t, stream, member)
	if len(th.Turns) != 3 || th.Turns[1].Request.TurnID != answering || th.Turns[1].Status() != "interrupted" || th.Turns[2].Request.TurnID != retried || th.Turns[2].Status() != "idle" {
		t.Fatalf("asker's thread: %+v", th)
	}
	if mine := f.Record(t, stream, member); mine.Turn != retried || !mine.Silent() {
		t.Fatalf("record: %+v", mine)
	}
	if end := f.Transition(t, stream, "shed-concluded-1"); end.Reason != "debate concluded by consensus after round 1: no objection stands" {
		t.Fatalf("conclusion: %+v", end)
	}
}

func TestAskingTurnInterruptedByAStopStillParksTheRound(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	var parked atomic.Bool
	f, c := newAskingFixtureWith(t, 1, 1, p, func(opts *journey.Options) {
		opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
			return op.Action == thread.TurnAction && !parked.Load()
		}
	})
	defer f.Stop(t)
	c.Release("1")
	member := intake.CommitteeAgent(1)
	first := shed.ObjectionID(1, member, 1)
	asked := make(chan struct{})
	asking := f.Member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if err := errors.Join(intakefixture.Objects(p, shed.Fit, "plan", "spec#1")(ctx, req, verified, tools), intakefixture.Asks(p, "1")(ctx, req, verified, tools)); err != nil {
			return err
		}
		close(asked)
		<-ctx.Done()
		return ctx.Err()
	})
	answering := f.Answer("1", intakefixture.Concedes(p, first))
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	select {
	case <-asked:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the member never asked")
	}
	f.Stop(t)
	f.Start(t)
	f.AwaitShed(t, stream, "waiting-1", "heard-1", "failed-1")
	parked.Store(true)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if park := f.Transition(t, stream, "shed-round-1-waiting-1"); park.Reason != fmt.Sprintf("round 1 against %s is parked: %s waits for the answer to question 1", intakefixture.PinOne, member) {
		t.Fatalf("park: %+v", park)
	}
	if ran := f.Ran(); ran[asking] != 1 || ran[answering] != 1 || ran[intake.RoundTurnID(1, member, 2)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	th := f.Thread(t, stream, member)
	if len(th.Turns) != 2 || th.Turns[0].Status() != "interrupted" || th.Turns[1].Request.TurnID != answering || th.Turns[1].Status() != "idle" {
		t.Fatalf("asker's thread: %+v", th)
	}
	if mine := f.Record(t, stream, member); mine.Turn != asking || mine.Failure != "" || len(mine.Objections) != 1 || len(mine.Concessions) != 1 {
		t.Fatalf("record: %+v", mine)
	}
	if end := f.Transition(t, stream, "shed-concluded-1"); end.Reason != "debate concluded by consensus after round 1: no objection stands" {
		t.Fatalf("conclusion: %+v", end)
	}
}

func TestAnswerQueuedBehindAReservedTurnRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)

	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		return intakefixture.QuestionResult(req, "session-chief", "Choice left for the planted turn"), nil
	}
	f.Engine.Mu.Unlock()
	member := intake.CommitteeAgent(1)
	asked := make(chan struct{})
	asking := f.Member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if err := intakefixture.Asks(p, "1")(ctx, req, verified, tools); err != nil {
			return err
		}
		close(asked)
		<-ctx.Done()
		return ctx.Err()
	})
	answering := f.Answer("1", intakefixture.Silent)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	select {
	case <-asked:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the member never asked")
	}

	intakefixture.Eventually(t, "the chief of staff's turns were never claimed", func() bool {
		th, err := f.Repository().Thread(stream, trace.Manager)
		fixturekit.Must(t, err)
		return !slices.ContainsFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Claim == nil && q.CompletedAt.IsZero() })
	})
	f.Stop(t)
	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = c.Turn
	f.Engine.Mu.Unlock()

	cfg, err := projectstore.LoadConfig(f.Opts.Config)
	fixturekit.Must(t, err)
	repo, err := trace.Open(cfg.Root, config.Project{ID: f.Project, Clone: f.Clone})
	fixturekit.Must(t, err)
	th, err := repo.Thread(stream, member)
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Status() != "interrupted" {
		t.Fatalf("asking turn after the stop: %+v", th.Turns)
	}
	reserved := intake.RoundTurnID(1, member, 2)
	second := th.Turns[0].Request
	second.ID, second.TurnID, second.At = "request_"+reserved, reserved, f.Clock.Now()
	_, err = repo.EnqueueTurn(ctx, second)
	fixturekit.Must(t, err)
	home := filepath.Dir(f.Clone)
	_, err = repo.ClaimTurn(ctx, stream, member, "token_"+reserved, filepath.Join(home, reserved), f.Clock.Now())
	fixturekit.Must(t, err)

	manager, err := repo.Thread(stream, trace.Manager)
	fixturekit.Must(t, err)
	if manager.Active != "" {
		fixturekit.Must(t, repo.AbandonTurn(ctx, stream, trace.Manager, manager.Active, f.Clock.Now()))
	}
	managerHeader := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_planted", Revision: 1, Project: f.Project, Workstream: stream, At: f.Clock.Now(), Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "planted"}
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: managerHeader, AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "planted", Profile: second.Profile, Prompt: "Answer"})
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.Manager, "token_planted", filepath.Join(home, "planted"), f.Clock.Now())
	fixturekit.Must(t, err)
	managerScope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Thread: trace.Manager, Turn: "planted", Role: trace.Manager}
	_, err = repo.AnswerQuestion(ctx, trace.Manager, managerScope, "1", intakefixture.AskedAnswer, []string{"charter#1"}, f.Clock.Now())
	fixturekit.Must(t, err)

	fixturekit.Must(t, repo.Close())
	repo, err = trace.Open(cfg.Root, config.Project{ID: f.Project, Clone: f.Clone})
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.AbandonTurn(ctx, stream, trace.Manager, "planted", f.Clock.Now()))
	planted, err := repo.Questions(stream)
	fixturekit.Must(t, err)
	if len(planted) != 1 || planted[0].State != trace.QuestionAnswered {
		t.Fatalf("planted questions: %+v", planted)
	}
	if delivered, err := questions.Deliver(ctx, repo, planted[0], func(string, string) (coreadapter.Profile, error) {
		return coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, nil
	}, f.Clock.Now()); err != nil || !delivered {
		t.Fatalf("deliver: %t %v", delivered, err)
	}
	if th, err := repo.Thread(stream, member); err != nil || len(th.Turns) != 3 || th.Turns[1].Claim == nil || th.Turns[1].Response != nil || th.Turns[2].Request.TurnID != answering {
		t.Fatalf("planted thread: %+v %v", th, err)
	}
	fixturekit.Must(t, repo.Close())
	f.Start(t)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	th = f.Thread(t, stream, member)
	if len(th.Turns) != 3 || th.Turns[1].Request.TurnID != reserved || th.Turns[1].Status() != "interrupted" || th.Turns[2].Request.TurnID != answering || th.Turns[2].Status() != "idle" {
		t.Fatalf("asker's thread: %+v", th)
	}
	if ran := f.Ran(); ran[asking] != 1 || ran[answering] != 1 || ran[reserved] != 0 || ran[intake.RoundTurnID(1, member, 3)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	if mine := f.Record(t, stream, member); mine.Turn != asking || !mine.Silent() {
		t.Fatalf("record: %+v", mine)
	}
}

func TestTurnThatFailedAfterAskingParksTheRound(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	c.Release("1")
	member := intake.CommitteeAgent(1)
	asking := f.Member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if err := intakefixture.Asks(p, "1")(ctx, req, verified, tools); err != nil {
			return err
		}
		return errors.New("the backend crashed after the question")
	})
	answering := f.Answer("1", intakefixture.Silent)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	th := f.Thread(t, stream, member)
	if len(th.Turns) != 2 || th.Turns[0].Status() != "failed" || th.Turns[1].Request.TurnID != answering || th.Turns[1].Status() != "idle" {
		t.Fatalf("asker's thread: %+v", th)
	}
	if ran := f.Ran(); ran[asking] != 1 || ran[answering] != 1 || ran[intake.RoundTurnID(1, member, 2)] != 0 {
		t.Fatalf("turns ran: %v", ran)
	}
	if mine := f.Record(t, stream, member); mine.Turn != asking || !mine.Silent() {
		t.Fatalf("record: %+v", mine)
	}
	if told := f.Transition(t, stream, "shed-round-1-heard"); told.Reason != fmt.Sprintf("round 1 against %s: 1 members heard, 0 objections, 0 concessions, 0 failed turns; 0 objections stand", intakefixture.PinOne) {
		t.Fatalf("heard: %+v", told)
	}
}

func TestMemberAsksAgainInItsAnswerTurn(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	f, c := newAskingFixture(t, 1, 1, p)
	defer f.Stop(t)
	c.Release("1")
	c.Release("2")
	member := intake.CommitteeAgent(1)
	ids := []string{shed.ObjectionID(1, member, 1), shed.ObjectionID(1, member, 2), shed.ObjectionID(1, member, 3)}
	asking := f.Member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		return errors.Join(intakefixture.Objects(p, shed.Fit, "plan", "spec#1")(ctx, req, verified, tools), intakefixture.Asks(p, "1")(ctx, req, verified, tools))
	})
	f.Answer("1", func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if recorded, id, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "acceptance", "part": "plan#dedupe", "argument": "No unit shows it.", "citations": []string{"spec#2"}}); err != nil || !recorded || id != ids[1] {
			p.Report("objection in the first answer turn: %v %q %v", recorded, id, err)
		}
		return intakefixture.Asks(p, "2")(ctx, req, verified, tools)
	})
	f.Answer("2", func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, "Answer to your question 2.") {
			p.Report("second answer prompt:\n%s", req.Prompt)
		}
		if recorded, id, err := intakefixture.ShedTool(ctx, tools, shed.ObjectTool, map[string]any{"kind": "size", "part": "plan#resume", "argument": "Too wide.", "citations": []string{"kb/entities.json#internal.trace"}}); err != nil || !recorded || id != ids[2] {
			p.Report("objection in the second answer turn: %v %q %v", recorded, id, err)
		}
		return intakefixture.Concedes(p, ids...)(ctx, req, verified, tools)
	})
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)

	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	first, second := f.RoundOperation(t, stream, 1), f.RoundOperation(t, stream, 2)
	parked := fmt.Sprintf("round 1 against %s is parked: %s waits for the answer to question 2", intakefixture.PinOne, member)
	if first.Result.Outcome != "waiting" || first.Result.Evidence != parked || second.Result.Outcome != "succeeded" {
		t.Fatalf("round operations: %v", intakefixture.Results(f.RoundOperations(t, stream)))
	}
	if park := f.Transition(t, stream, "shed-round-1-waiting-2"); park.Cause != first.Operation.ID || park.From != "round-1" || park.To != "waiting-1" || park.Reason != parked {
		t.Fatalf("second park: %+v", park)
	}
	if resumption := f.Transition(t, stream, "shed-round-1-resume-2"); resumption.Cause != "shed-round-1-waiting-2" || resumption.From != "waiting-1" || resumption.To != "round-1" {
		t.Fatalf("second resumption: %+v", resumption)
	}
	if in, err := intake.DecodeRound(second.Operation); err != nil || in != (intake.RoundInput{Round: 1, Spec: 1, Plan: 1, Resume: 2}) {
		t.Fatalf("second resumption input %+v %v", in, err)
	}
	if told := f.Transition(t, stream, "shed-round-1-heard"); told.Cause != second.Operation.ID {
		t.Fatalf("heard: %+v", told)
	}
	mine := f.Record(t, stream, member)
	if mine.Turn != asking || len(mine.Objections) != 3 || len(mine.Concessions) != 3 || mine.Failure != "" {
		t.Fatalf("record: %+v", mine)
	}
	for i, o := range mine.Objections {
		if o.ID != ids[i] {
			t.Fatalf("objection %d is %s", i+1, o.ID)
		}
	}
	if th := f.Thread(t, stream, member); len(th.Turns) != 3 || th.Turns[0].Status() != "waiting" || th.Turns[1].Status() != "waiting" || th.Turns[2].Status() != "idle" {
		t.Fatalf("asker's thread: %+v", th)
	}
}

func TestRoundResumesOnlyWhenEveryAskerHasItsAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	f, c := newAskingFixture(t, 2, 1, p)
	defer f.Stop(t)
	c.Release("1")
	one, two := intake.CommitteeAgent(1), intake.CommitteeAgent(2)
	firstAsked := make(chan struct{})
	f.Member(1, 1, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		defer close(firstAsked)
		return intakefixture.Asks(p, "1")(ctx, req, verified, tools)
	})
	f.Member(1, 2, 1, func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error {
		select {
		case <-firstAsked:
		case <-ctx.Done():
			return ctx.Err()
		}
		return intakefixture.Asks(p, "2")(ctx, req, verified, tools)
	})
	answerOne, answerTwo := f.Answer("1", intakefixture.Silent), f.Answer("2", intakefixture.Silent)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "waiting-1", "heard-1", "failed-1")
	p.Check(t)
	parked := fmt.Sprintf("round 1 against %s is parked: %s waits for the answer to question 1; %s waits for the answer to question 2", intakefixture.PinOne, one, two)
	if park := f.Transition(t, stream, "shed-round-1-waiting-1"); park.Reason != parked {
		t.Fatalf("park: %+v", park)
	}

	queued := f.AwaitTurn(t, stream, one, answerOne)
	f.AwaitQuestion(t, stream, "2", trace.QuestionEscalated)
	fixturekit.Must(t, (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: f.Repository()}).Pass(ctx))
	if state, err := f.Repository().Workflow(stream, intake.ShedSubject); err != nil || state.Value != "waiting-1" || !queued.CompletedAt.IsZero() || len(f.RoundOperations(t, stream)) != 1 {
		t.Fatalf("shed with one answer queued: %+v %v", state, err)
	}
	if ran := f.Ran(); ran[answerOne] != 0 {
		t.Fatalf("the first answer ran before the second arrived: %v", ran)
	}
	f.Rule(t, "2")
	f.AwaitShed(t, stream, "concluded-1")
	p.Check(t)
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "waiting-1", "round-1", "heard-1", "concluded-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if ran := f.Ran(); ran[answerOne] != 1 || ran[answerTwo] != 1 {
		t.Fatalf("turns ran: %v", ran)
	}
	if told := f.Transition(t, stream, "shed-round-1-heard"); told.Reason != fmt.Sprintf("round 1 against %s: 2 members heard, 0 objections, 0 concessions, 0 failed turns; 0 objections stand", intakefixture.PinOne) {
		t.Fatalf("heard: %+v", told)
	}
	for _, member := range []string{one, two} {
		if mine := f.Record(t, stream, member); !mine.Silent() {
			t.Fatalf("record of %s: %+v", member, mine)
		}
	}
}

func TestAbandoningAParkedRoundLeavesItParked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &intakefixture.Faults{}
	var managerReviewHeld atomic.Bool
	managerReviewHeld.Store(true)
	f, c := newAskingFixtureWith(t, 1, 1, p, func(opts *journey.Options) {
		opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
			return op.Action == thread.TurnAction && managerReviewHeld.Load()
		}
	})
	defer f.Stop(t)
	journey.InspectForTest(f.S).HoldAnswerDelivery()
	hold := c.Hold("1")
	member := intake.CommitteeAgent(1)
	f.Member(1, 1, 1, intakefixture.Asks(p, "1"))
	answering := f.Answer("1", intakefixture.Silent)
	stream := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, stream, "waiting-1", "heard-1", "failed-1")

	managerReviewHeld.Store(false)

	f.AwaitQuestion(t, stream, "1", trace.QuestionAnswered)
	if _, err := f.C.Abandon(ctx, stream, "Superseded."); err != nil {
		t.Fatal(err)
	}
	close(hold)

	repo := f.Repository()
	drivePass(t, f.S, repo, func() error {
		if err := f.S.Answers(f.S.Current(), repo).Pass(ctx); err != nil {
			return err
		}
		return (&intake.Debate{S: f.S.Intake().DebateDependencies(), Repository: repo}).Pass(ctx)
	})
	if moves, want := f.ShedMoves(t, stream), []string{"round-1", "waiting-1"}; !slices.Equal(moves, want) {
		t.Fatalf("shed went %v, want %v", moves, want)
	}
	if th := f.Thread(t, stream, member); len(th.Turns) != 1 || th.Turns[0].Status() != "waiting" {
		t.Fatalf("asker's thread after the abandonment: %+v", th)
	}
	if ran := f.Ran(); ran[answering] != 0 {
		t.Fatalf("the answer ran: %v", ran)
	}
	if records, err := shed.Records(f.Repository(), stream); err != nil || len(records) != 0 {
		t.Fatalf("records: %+v %v", records, err)
	}
	if feature, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.AbandonedState {
		t.Fatalf("feature: %+v %v", feature, err)
	}
	if q := f.Question(t, stream, "1"); q.State != trace.QuestionAnswered {
		t.Fatalf("question: %+v", q)
	}
}
