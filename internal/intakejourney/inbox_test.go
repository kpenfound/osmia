package intakejourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	slices "slices"
	strconv "strconv"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	bundle "github.com/kpenfound/osmia/internal/bundle"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	envelope "github.com/kpenfound/osmia/internal/envelope"
	"github.com/kpenfound/osmia/internal/projectstore"
	questions "github.com/kpenfound/osmia/internal/questions"
	reconcile "github.com/kpenfound/osmia/internal/reconcile"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	seal "github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestQuickReplyEligibility(t *testing.T) {
	t.Parallel()
	for _, word := range []string{"push", "merge", "deliver", "abandon", "force", "delete", "rebase", "overrule", "deploy", "ratify", "veto", "revert", "reset", "discard", "PUSHES", "merged", "ratified", "vetoing", "abandoned", "force-push", "Merge,please"} {
		t.Run(word, func(t *testing.T) {
			entry := trace.InboxEntry{State: trace.QuestionEscalated, Recommendation: "Please " + word + " this.", Questions: []trace.QuestionState{{}}}
			if got := intake.InboxView(entry).QuickReply; got != "" {
				t.Fatalf("quick reply for %q: %q", word, got)
			}
		})
	}
	for _, recommendation := range []string{"", " \n", "Discuss the merger, pushover, forceful work and released notes.", "Use the files as written."} {
		entry := trace.InboxEntry{State: trace.QuestionEscalated, Recommendation: recommendation, Questions: []trace.QuestionState{{}}}
		want := recommendation
		if strings.TrimSpace(want) == "" {
			want = ""
		}
		if got := intake.InboxView(entry).QuickReply; got != want {
			t.Fatalf("quick reply for %q: %q, want %q", recommendation, got, want)
		}
	}
	for _, decision := range []string{"ratification", "contested-unit", "amendment-decision", "delivery-approval"} {
		entry := trace.InboxEntry{State: decision, Recommendation: "Proceed with the plan.", Questions: []trace.QuestionState{{}}}
		if got := intake.InboxView(entry).QuickReply; got != "" {
			t.Fatalf("%s quick reply: %q", decision, got)
		}
	}
	if got := intake.InboxView(trace.InboxEntry{State: trace.QuestionEscalated, Recommendation: "Proceed with the plan."}).QuickReply; got != "" {
		t.Fatalf("entry without questions has quick reply %q", got)
	}
}

func TestOwnerRulingResumesTheAskersAcrossRestarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewQuestionFixture(t, "qi-", "", "# Charter\n\n1. Keep state in files under the root.\n", []intakefixture.QuestionAsker{
		{Stream: fixturekit.Stream, Agent: fixturekit.DemoAgent, Thread: fixturekit.DemoThread, Role: "mason", Turn: "build"}, {Stream: fixturekit.Stream, Agent: "agent_reviewer", Thread: "thread_reviewer", Role: "reviewer", Turn: "review"}, {Stream: intakefixture.Quiet, Agent: "agent_other", Thread: "thread_other", Role: "mason", Turn: "build_other"},
	})
	asker := func(question string) fixturekit.DemoTurn {
		return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			f.Mu.Lock()
			defer f.Mu.Unlock()
			if err := f.Use(ctx, tools, req.Name, "ask", map[string]any{"question": question}); err != nil {
				return nil, err
			}
			return intakefixture.QuestionResult(req, "session-"+req.Name, "Asked"), nil
		}
	}
	f.Engine.Turns["build"] = asker("Where does state live?")
	f.Engine.Turns["review"] = asker("Is the log format fixed?")
	f.Engine.Turns["build_other"] = asker("May I add a dependency?")

	resumed := func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		f.Mu.Lock()
		defer f.Mu.Unlock()
		if names, err := intakefixture.ToolNames(ctx, tools); err != nil || !slices.Equal(names, []string{"ask", "file_read"}) {
			f.Problem("%s tools %v: %v", req.Name, names, err)
		}
		if !strings.Contains(req.Prompt, "Work: ") {
			f.Problem("%s does not continue the asking thread:\n%s", req.Name, req.Prompt)
		}
		f.Results["resumed"] = append(f.Results["resumed"], req.Prompt[strings.Index(req.Prompt, "The owner ruled"):])
		return intakefixture.QuestionResult(req, "session-"+req.Name, "Built"), nil
	}
	f.Engine.Turns["answer_1"], f.Engine.Turns["answer_2"] = resumed, resumed

	relayed := make(chan struct{})
	section, sectionErr := envelope.Render(envelope.Section{Name: "owner_response", Text: "Both are part of the contract."}, envelope.Section{Name: "returned_answer", Text: "State and the log format are part of the contract."})
	if sectionErr != nil {
		t.Fatal(sectionErr)
	}
	notice := "- workstreams/" + string(fixturekit.Stream) + "/questions/1/rulings.jsonl (record 1 revision 2, workstream " + string(fixturekit.Stream) + ")\n" + section
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		f.Mu.Lock()
		switch {

		case strings.Contains(req.Prompt, ", escalation_1 (questions 1, 2): Both are part of the contract."):
			select {
			case <-relayed:

				f.Mu.Unlock()
				return intakefixture.QuestionResult(req, "session-chief", "Already relayed"), nil
			default:
			}
			if strings.Contains(req.Prompt, "<<< osmia:owner_response") {
				f.Problem("a notice before any notify ruling:\n%s", req.Prompt)
			}
			for _, args := range []map[string]any{
				{"question": "2", "text": "State and the log format are part of the contract.", "scope": "notify"},
				{"question": "1", "text": "Again.", "scope": "local"},
			} {
				if err := f.Use(ctx, tools, "relay", "relay_ruling", args); err != nil {
					f.Problem("relay_ruling: %v", err)
					f.Mu.Unlock()
					return nil, err
				}
			}
			f.Mu.Unlock()

			close(relayed)
			<-ctx.Done()
			return nil, ctx.Err()
		case strings.Contains(req.Prompt, ", escalation_1 (questions 1): No new dependencies."):
			defer f.Mu.Unlock()

			if !strings.Contains(req.Prompt, "## Notices\n"+notice) {
				f.Problem("event turn context lacks the notice:\n%s", req.Prompt)
			}
			return intakefixture.QuestionResult(req, "session-chief", "Relayed"), f.Use(ctx, tools, "relay-local", "relay_ruling", map[string]any{"question": "1", "text": "Add no dependency.", "scope": "local"})
		case strings.Contains(req.Prompt, "Where does state live?") && strings.Contains(req.Prompt, "Is the log format fixed?") && strings.Contains(req.Prompt, "is open"):
			defer f.Mu.Unlock()
			return intakefixture.QuestionResult(req, "session-chief", "Escalated"), f.Use(ctx, tools, "escalate", "escalate", map[string]any{"questions": []string{"1", "2"},
				"rephrasing": "Are state files and the log format part of the contract?", "blocked": "The upload unit and its review.", "options": []string{"Both fixed", "Both free"}, "recommendation": "Both fixed."})
		case strings.Contains(req.Prompt, "is open, asked by the mason: May I add a dependency?"):
			defer f.Mu.Unlock()
			return intakefixture.QuestionResult(req, "session-chief", "Escalated"), f.Use(ctx, tools, "escalate", "escalate", map[string]any{"questions": []string{"1"},
				"rephrasing": "May the index unit add a dependency?", "blocked": "The index unit.", "options": []string{}, "recommendation": "No."})
		}
		f.Problem("unexpected chief-of-staff turn:\n%s", req.Prompt)
		f.Mu.Unlock()
		return intakefixture.QuestionResult(req, "session-chief", "Nothing to do"), nil
	}

	states := func(r *trace.Repository, ws config.WorkstreamID) map[string]string {
		t.Helper()
		list, err := r.Questions(ws)
		fixturekit.Must(t, err)
		out := map[string]string{}
		for _, q := range list {
			out[q.Asked.ID] = q.State
		}
		return out
	}
	thread := func(r *trace.Repository, ws config.WorkstreamID, agent string) trace.Thread {
		t.Helper()
		th, err := r.Thread(ws, agent)
		fixturekit.Must(t, err)
		return th
	}
	turnsOf := func(r *trace.Repository, ws config.WorkstreamID, agent string) (out []string) {
		for _, q := range thread(r, ws, agent).Turns {
			out = append(out, q.Request.TurnID)
		}
		return out
	}
	runs := func() map[string]int {
		f.Engine.Mu.Lock()
		defer f.Engine.Mu.Unlock()
		out := map[string]int{}
		for _, name := range f.Engine.Runs {
			if strings.HasPrefix(name, "events_") {
				name = "events"
			}
			out[name]++
		}
		return out
	}
	lifetime := func() (*journey.Service, *journey.Client, *trace.Repository) {
		t.Helper()
		s, err := journey.Start(ctx, f.Opts)
		fixturekit.Must(t, err)
		return s, journey.NewClient(s.Socket()), <-f.Lives
	}
	stop := func(s *journey.Service, c *journey.Client) {
		t.Helper()
		c.Close()
		fixturekit.Must(t, s.Close())
	}

	s, c, repo := lifetime()
	f.Settle(t, s)
	f.Clock.Advance(time.Minute)
	f.Settle(t, s)
	inbox, err := c.Inbox(ctx)
	fixturekit.Must(t, err)
	if len(inbox.Entries) != 2 || inbox.Entries[0].Number != 1 || inbox.Entries[1].Number != 2 {
		t.Fatalf("inbox: %+v", inbox)
	}
	batch, single := inbox.Entries[0], inbox.Entries[1]
	if batch.Workstream != fixturekit.Stream {
		batch, single = single, batch
	}
	wantBatch := api.InboxEntry{Kind: api.InboxEscalation, Project: fixturekit.Project, Number: batch.Number, Workstream: fixturekit.Stream, Batch: "escalation_1", Question: "Are state files and the log format part of the contract?", Blocked: "The upload unit and its review.",
		Options: []string{"Both fixed", "Both free"}, Recommendation: "Both fixed.", QuickReply: "Both fixed.", OpenedAt: f.Clock.Now(), Answer: api.InboxAnswer{Method: "POST", Path: "/v1/inbox/" + strconv.Itoa(batch.Number), Body: map[string]any{"project": string(fixturekit.Project)}},
		Asked: []api.InboxQuestion{{ID: "1", AskedBy: fixturekit.DemoAgent, Question: "Where does state live?"}, {ID: "2", AskedBy: "agent_reviewer", Question: "Is the log format fixed?"}}}

	if batch.Asked[0].Question != "Where does state live?" {
		wantBatch.Asked[0].Question, wantBatch.Asked[1].Question = wantBatch.Asked[1].Question, wantBatch.Asked[0].Question
		wantBatch.Asked[0].AskedBy, wantBatch.Asked[1].AskedBy = wantBatch.Asked[1].AskedBy, wantBatch.Asked[0].AskedBy
	}
	if !reflect.DeepEqual(batch, wantBatch) {
		t.Fatalf("batch entry:\n%+v\nwant\n%+v", batch, wantBatch)
	}
	if single.Workstream != intakefixture.Quiet || single.Batch != "escalation_1" || single.Question != "May the index unit add a dependency?" || single.Options == nil || len(single.Options) != 0 || len(single.Asked) != 1 || single.Asked[0].AskedBy != "agent_other" {
		t.Fatalf("single entry: %+v", single)
	}
	if got, err := c.Status(ctx, fixturekit.Stream); err != nil || !reflect.DeepEqual(got.Gates, []trace.OwnerGate{{Kind: "escalation", Reference: strconv.Itoa(batch.Number)}}) {
		t.Fatalf("status gates: %+v %v", got.Gates, err)
	}
	if list, err := c.Statuses(ctx); err != nil || len(list.Workstreams) != 2 {
		t.Fatalf("status list: %+v %v", list, err)
	} else {
		for _, ws := range list.Workstreams {
			if len(ws.Gates) != 1 || ws.Gates[0].Kind != "escalation" {
				t.Fatalf("list gates: %+v", ws)
			}
		}
	}

	traceDir, err := f.Cfg.Root.ProjectTrace(fixturekit.Project)
	fixturekit.Must(t, err)
	head := fixturekit.TraceCommits(t, traceDir)
	_, err = c.Answer(ctx, 9, "Both are part of the contract.", "")
	intakefixture.ApiError(t, err, api.Validation, "there is no inbox entry 9; list the entries with osmia inbox")
	_, err = c.Answer(ctx, batch.Number, " \n", "")
	intakefixture.ApiError(t, err, api.Validation, "text must not be empty")
	for _, number := range []string{"0", "-1", "01", "x", "1.5"} {
		intakefixture.ApiError(t, c.Do(ctx, "POST", api.Prefix+"/inbox/"+number, api.AnswerRequest{Text: "x"}, new(api.AnswerResponse)), api.Validation, "inbox entry must be a number from osmia inbox")
	}
	if got := fixturekit.TraceCommits(t, traceDir); got != head {
		t.Fatalf("a refused answer committed: %d commits, was %d", got, head)
	}
	answered, err := c.Answer(ctx, batch.Number, "Both are part of the contract.", "")
	fixturekit.Must(t, err)
	if got, err := c.Status(ctx, fixturekit.Stream); err != nil || len(got.Gates) != 0 {
		t.Fatalf("ruled status gates: %+v %v", got.Gates, err)
	}
	if !reflect.DeepEqual(answered, api.AnswerResponse{Number: batch.Number, Workstream: fixturekit.Stream, Batch: "escalation_1", Questions: []string{"1", "2"}, Ruling: "Both are part of the contract.", At: f.Clock.Now()}) {
		t.Fatalf("answer: %+v", answered)
	}
	_, err = c.Answer(ctx, batch.Number, "Neither is.", "")
	intakefixture.ApiError(t, err, api.Conflict, "inbox entry "+strconv.Itoa(batch.Number)+" is already answered")
	inbox, err = c.Inbox(ctx)
	fixturekit.Must(t, err)
	if len(inbox.Entries) != 1 || !reflect.DeepEqual(inbox.Entries[0], single) {
		t.Fatalf("inbox after the answer: %+v", inbox)
	}
	list, err := repo.Questions(fixturekit.Stream)
	fixturekit.Must(t, err)
	masonQuestion := ""
	for _, q := range list {
		if q.Asked.AskedBy.ID == fixturekit.DemoAgent {
			masonQuestion = q.Asked.ID
		}
		if r := q.Ruling; q.State != trace.QuestionRuled || r == nil || r.OwnerResponse != "Both are part of the contract." || r.ReturnedAnswer != "" || r.Actor != intake.OwnerActor || r.Decision != trace.DecisionRuling {
			t.Fatalf("ruled question %s: %+v", q.Asked.ID, q)
		}
	}
	if masonQuestion == "" {
		t.Fatal("mason's question was not recorded")
	}

	target := runtime.Target{Scope: "workstream", Project: fixturekit.Project, Workstream: fixturekit.Stream}
	intakefixture.Mutation(t, c, "PUT", "pause", api.PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	started := map[string]int{"build": 1, "review": 1, "build_other": 1, "events": 2}
	if got := runs(); !reflect.DeepEqual(got, started) {
		t.Fatalf("first lifetime runs: %v", got)
	}
	stop(s, c)

	s, c, repo = lifetime()
	f.Settle(t, s)
	if got := runs(); !reflect.DeepEqual(got, started) {
		t.Fatalf("restart after the ruling ran: %v", got)
	}
	f.Clock.Advance(time.Minute)
	deadline := time.After(fixturekit.DemoTimeout)
	for waiting := true; waiting; {
		select {
		case f.Ticks <- f.Clock.Now():
		case <-relayed:
			waiting = false
		case <-deadline:
			s.Close()
			t.Fatalf("chief of staff did not relay the ruling; runs %v, fake agents saw %v", runs(), f.Problems)
		}
	}
	if got := states(repo, fixturekit.Stream); !reflect.DeepEqual(got, map[string]string{"1": trace.QuestionAnswered, "2": trace.QuestionAnswered}) {
		t.Fatalf("questions after the relay: %v", got)
	}

	wait, closeWait := journey.ServiceChanges(t, s)
	queuedBy := time.Now().Add(fixturekit.DemoTimeout)
	for {
		th := thread(repo, fixturekit.Stream, fixturekit.DemoAgent)
		if len(th.Turns) == 2 && th.Turns[1].Request.TurnID == questions.TurnID(masonQuestion) && th.Turns[1].Request.Actor == questions.Actor {
			break
		}
		if len(th.Turns) > 2 || time.Now().After(queuedBy) {
			closeWait()
			t.Fatalf("ruling delivered other than by the answer pass: %v", turnsOf(repo, fixturekit.Stream, fixturekit.DemoAgent))
		}
		wait()
	}
	closeWait()
	stop(s, c)

	s, c, repo = lifetime()
	f.Settle(t, s)
	queued := func(r *trace.Repository) {
		t.Helper()
		mason, reviewer := thread(r, fixturekit.Stream, fixturekit.DemoAgent), thread(r, fixturekit.Stream, "agent_reviewer")
		if len(mason.Turns) != 2 || len(reviewer.Turns) != 2 || mason.Turns[1].Claim != nil || reviewer.Turns[1].Claim != nil {
			t.Fatalf("held answer turns: mason %v, reviewer %v", turnsOf(r, fixturekit.Stream, fixturekit.DemoAgent), turnsOf(r, fixturekit.Stream, "agent_reviewer"))
		}
	}
	queued(repo)
	stop(s, c)

	s, c, repo = lifetime()
	f.Settle(t, s)
	queued(repo)
	if got := runs(); got["answer_1"] != 0 || got["answer_2"] != 0 {
		t.Fatalf("held answer turns ran: %v", got)
	}
	intakefixture.Mutation(t, c, "DELETE", "pause", api.ClearPauseRequest(target))
	f.Settle(t, s)
	for agent, thread := range map[string]string{fixturekit.DemoAgent: fixturekit.DemoThread, "agent_reviewer": "thread_reviewer"} {
		th, err := repo.Thread(fixturekit.Stream, agent)
		fixturekit.Must(t, err)
		if len(th.Turns) != 2 || th.Parked() || th.Turns[1].Request.ThreadID != thread || th.Turns[1].Response == nil || th.Turns[1].Status() != "idle" {
			t.Fatalf("%s after the ruling: parked %v, turns %+v", agent, th.Parked(), th.Turns)
		}
	}
	_, err = c.Answer(ctx, single.Number, "No new dependencies.", "")
	fixturekit.Must(t, err)
	f.Clock.Advance(time.Minute)
	f.Settle(t, s)
	f.Settle(t, s)
	if th := thread(repo, intakefixture.Quiet, "agent_other"); len(th.Turns) != 2 || th.Parked() || th.Turns[1].Request.ThreadID != "thread_other" {
		t.Fatalf("other workstream's asker: parked %v, turns %v", th.Parked(), turnsOf(repo, intakefixture.Quiet, "agent_other"))
	}
	inbox, err = c.Inbox(ctx)
	fixturekit.Must(t, err)
	if len(inbox.Entries) != 0 || inbox.Entries == nil {
		t.Fatalf("inbox after every ruling: %+v", inbox)
	}

	for _, ws := range []config.WorkstreamID{fixturekit.Stream, intakefixture.Quiet} {
		b, err := s.Context().Assemble(ctx, fixturekit.Project, bundle.Scope{Workstream: ws})
		fixturekit.Must(t, err)
		if len(b.Notices) != 1 || !strings.Contains(b.Render(), "## Notices\n"+notice) {
			t.Fatalf("notices of %s: %+v", ws, b.Notices)
		}
	}
	stop(s, c)

	s, c, repo = lifetime()
	f.Clock.Advance(24 * time.Hour)
	f.Settle(t, s)
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"build": 1, "review": 1, "build_other": 1, "events": 5, "answer_1": 2, "answer_2": 1}) {
		t.Fatalf("runs: %v", got)
	}
	if got := turnsOf(repo, fixturekit.Stream, "agent_reviewer"); len(got) != 2 {
		t.Fatalf("reviewer turns: %v", got)
	}
	stop(s, c)

	f.Mu.Lock()
	defer f.Mu.Unlock()
	slices.Sort(f.Results["resumed"])
	ruled := func(id, question, answer string) string {
		ownerWords := "Both are part of the contract."
		if answer == "Add no dependency." {
			ownerWords = "No new dependencies."
		}
		return questions.Prompt(trace.Question{Header: trace.Header{ID: id}, Question: question}, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: ownerWords, ReturnedAnswer: answer})
	}
	wantResumed := []string{ruled("1", wantBatch.Asked[0].Question, "State and the log format are part of the contract."), ruled("1", "May I add a dependency?", "Add no dependency."),
		ruled("2", wantBatch.Asked[1].Question, "State and the log format are part of the contract.")}
	slices.Sort(wantResumed)
	if !slices.Equal(f.Results["resumed"], wantResumed) {
		t.Fatalf("resumed prompts:\n%q\nwant\n%q", f.Results["resumed"], wantResumed)
	}
	if got, want := f.Results["relay"], []string{
		`{"recorded":true,"questions":["1","2"],"scope":"notify","next":"The ruling is delivered to each asker as its next turn."}`,
		`{"recorded":false,"reason":"question 1 is already answered"}`,
	}; !slices.Equal(got, want) {
		t.Fatalf("relay results: %v", got)
	}

	if got := slices.Sorted(slices.Values(f.Results["escalate"])); !slices.Equal(got, []string{`{"recorded":true,"batch":"escalation_1","questions":["1","2"]}`, `{"recorded":true,"batch":"escalation_1","questions":["1"]}`}) {
		t.Fatalf("escalation results: %v", got)
	}
	if got := f.Results["relay-local"]; !slices.Equal(got, []string{`{"recorded":true,"questions":["1"],"scope":"local","next":"The ruling is delivered to each asker as its next turn."}`}) {
		t.Fatalf("local relay result: %v", got)
	}
	if len(f.Problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(f.Problems, "\n"))
	}
}

func TestInboxLeavesOutAbandonedWorkstreams(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "qj-")
	fixturekit.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := journey.FixtureAt(t, home)
	cfg, err := projectstore.LoadConfig(opts.Config)
	fixturekit.Must(t, err)
	clock := &fixturekit.FixedClock{At: fixturekit.DemoStart}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), intake.OwnerActor)
	fixturekit.Must(t, err)
	defer repo.Close()
	for _, ws := range []config.WorkstreamID{fixturekit.Stream, intakefixture.Quiet} {
		fixturekit.Must(t, repo.CreateWorkstream(ctx, ws, clock.Now(), intake.OwnerActor))
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: fixturekit.DemoAgent, Project: fixturekit.Project, Workstream: ws, At: clock.Now(), Actor: intake.OwnerActor, Cause: "workstream_created"}, Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}
		fixturekit.Must(t, repo.CreateThread(ctx, identity))
		_, err := repo.Ask(ctx, fixturekit.DemoAgent, fixturekit.ClaimTurn(t, repo, home, ws, fixturekit.DemoAgent, fixturekit.DemoThread, "build", clock.Now()), "Where does state live?", clock.Now())
		fixturekit.Must(t, err)
		_, err = repo.EscalateQuestions(ctx, trace.ChiefOfStaff, fixturekit.ClaimTurn(t, repo, home, ws, trace.ChiefOfStaff, trace.ChiefOfStaff, "events", clock.Now()),
			trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "Where should state live?", Blocked: "The unit.", Recommendation: "In files."}, clock.Now())
		fixturekit.Must(t, err)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: intake.AbandonTransition, Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: clock.Now(), Actor: intake.OwnerActor, Cause: intake.AbandonTransition}
	_, err = repo.SetFeatureState(ctx, h, intake.AbandonedState, "gone")
	fixturekit.Must(t, err)

	s := &journey.Service{Cfg: cfg, Projects: []*journey.ActiveProject{journey.RuntimeFor(repo)}, Options: journey.Options{Options: service.Options{Reconciliation: reconcile.Options{Now: clock.Now}}}}
	inbox, apiErr := s.Inbox(ctx)
	if apiErr != nil || len(inbox.Entries) != 1 || inbox.Entries[0].Number != 2 || inbox.Entries[0].Workstream != intakefixture.Quiet {
		t.Fatalf("inbox: %+v %v", inbox, apiErr)
	}
	_, apiErr = s.Answer(ctx, "1", api.AnswerRequest{Text: "In files."})
	if apiErr == nil || apiErr.Code != api.Conflict || apiErr.Message != "inbox entry 1 belongs to abandoned workstream "+string(fixturekit.Stream)+" and takes no ruling" {
		t.Fatalf("answer to an abandoned workstream's entry: %v", apiErr)
	}
	if state, err := repo.Workflow(fixturekit.Stream, trace.QuestionSubject("1")); err != nil || state.Value != trace.QuestionEscalated {
		t.Fatalf("abandoned workstream's question: %+v %v", state, err)
	}
	if out, apiErr := s.Answer(ctx, "2", api.AnswerRequest{Text: "In files."}); apiErr != nil || out.Workstream != intakefixture.Quiet {
		t.Fatalf("answer: %+v %v", out, apiErr)
	}

	idle := &journey.Service{Cfg: cfg}
	if inbox, apiErr := idle.Inbox(ctx); apiErr != nil || inbox.Entries == nil || len(inbox.Entries) != 0 {
		t.Fatalf("inbox without a trace: %+v %v", inbox, apiErr)
	}
	if _, apiErr := idle.Answer(ctx, "1", api.AnswerRequest{Text: "In files."}); apiErr == nil || apiErr.Code != api.Validation || apiErr.Message != "there is no inbox entry 1; list the entries with osmia inbox" {
		t.Fatalf("answer without a trace: %v", apiErr)
	}

	empty, _ := journey.ProjectFixture(t)
	_, c := journey.StartT(t, empty)
	if inbox, err := c.Inbox(ctx); err != nil || inbox.Entries == nil || len(inbox.Entries) != 0 {
		t.Fatalf("inbox without a project: %+v %v", inbox, err)
	}
	_, err = c.Answer(ctx, 1, "In files.", "")
	intakefixture.ApiError(t, err, api.NoProject, "no project is configured; add one with osmia project add")
}

func TestInboxListsEveryOpenDecision(t *testing.T) {
	t.Parallel()
	f, assembled, repository, report := journey.DeliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	p := repository.Project()
	at := time.Now().UTC().Add(time.Minute)
	tick := func(n int) time.Time { return at.Add(time.Duration(n) * time.Second) }
	if state, err := repository.Workflow(assembled, trace.FeatureSubject); err != nil || state.Value != intake.AssembledState {
		t.Fatalf("fixture workstream: %+v %v", state, err)
	}
	escalating, shedding, contesting := config.WorkstreamID("w_"+strings.Repeat("1", 32)), config.WorkstreamID("w_"+strings.Repeat("2", 32)), config.WorkstreamID("w_"+strings.Repeat("3", 32))
	skipping := config.WorkstreamID("w_" + strings.Repeat("4", 32))
	for _, ws := range []config.WorkstreamID{escalating, shedding, contesting, skipping} {
		fixturekit.Must(t, repository.CreateWorkstream(ctx, ws, at, intake.OwnerActor))
	}
	header := func(ws config.WorkstreamID, schema, id string, when time.Time, actor trace.Actor) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: p, Workstream: ws, At: when, Actor: actor, Cause: "test"}
	}
	move := func(ws config.WorkstreamID, subject, id, to string, when time.Time, actor trace.Actor, docs ...trace.Document) {
		t.Helper()
		state, err := repository.Workflow(ws, subject)
		fixturekit.Must(t, err)
		h := header(ws, "osmia.trace.transition", id, when, actor)
		tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: to, Reason: "moved to " + to}}
		if len(docs) > 0 {
			_, err = repository.RecordDocumentsWith(ctx, docs, tx)
		} else {
			_, err = repository.Transact(ctx, tx)
		}
		fixturekit.Must(t, err)
	}
	claim := func(ws config.WorkstreamID, agent, thread, turn string) coreadapter.Scope {
		t.Helper()
		h := header(ws, "osmia.trace.turn-request", "request_"+turn, at, intake.OwnerActor)
		_, err := repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: agent, ThreadID: thread, TurnID: turn, Profile: coreadapter.Profile{Name: "other", Backend: "codex", Model: "other"}, Prompt: "Work"})
		fixturekit.Must(t, err)
		_, err = repository.ClaimTurn(ctx, ws, agent, "token_"+turn, filepath.Join(t.TempDir(), turn), at)
		fixturekit.Must(t, err)
		th, err := repository.Thread(ws, agent)
		fixturekit.Must(t, err)
		return coreadapter.Scope{Project: string(p), Workstream: string(ws), Thread: thread, Turn: turn, Role: th.Identity.Role}
	}

	fixturekit.Must(t, repository.CreateThread(ctx, trace.Agent{Header: header(escalating, "osmia.trace.agent", fixturekit.DemoAgent, at, intake.OwnerActor), Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}))
	_, err := repository.Ask(ctx, fixturekit.DemoAgent, claim(escalating, fixturekit.DemoAgent, fixturekit.DemoThread, "build"), "Where does state live?", tick(1))
	fixturekit.Must(t, err)
	_, err = repository.EscalateQuestions(ctx, trace.ChiefOfStaff, claim(escalating, trace.ChiefOfStaff, trace.ChiefOfStaff, "events"),
		trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "Where should state live?", Blocked: "The unit.", Options: []string{"Files", "A database"}, Recommendation: "In files."}, tick(1))
	fixturekit.Must(t, err)

	_, err = repository.SetFeatureState(ctx, header(shedding, "osmia.trace.transition", "in-shed", tick(2), intake.OwnerActor), intake.InShedState, "handed in")
	fixturekit.Must(t, err)
	move(shedding, intake.ShedSubject, "shed-concluded-1", "concluded-1", tick(2), intake.ShedActor)
	packetOf := func(ws config.WorkstreamID, skipped bool, round, revision int, recommendation string, when time.Time) trace.Document {
		content, err := shed.EncodePacket(shed.Packet{Version: shed.Version, Round: round, Revision: shed.Pin{Spec: 1, Plan: round}, Skipped: skipped, Conclusion: "no objection stands", Dissent: []shed.Entry{}, Recommendation: recommendation})
		fixturekit.Must(t, err)
		h := header(ws, "osmia.trace.document", shed.PacketDocumentID(round), when, intake.ShedActor)
		h.Revision, h.Cause = revision, "ratification-packet"
		return trace.Document{Header: h, Path: shed.PacketPath(round), Content: string(content)}
	}
	packet := func(round, revision int, recommendation string, when time.Time) trace.Document {
		return packetOf(shedding, false, round, revision, recommendation, when)
	}
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{packet(1, 1, "ratify: no objection stands", tick(2))}))
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{packet(1, 2, "ratify: nothing blocks, and 1 objection stands as advice on the record", tick(2))}))

	skippedAt := at.Add(2500 * time.Millisecond)
	_, err = repository.SetFeatureState(ctx, header(skipping, "osmia.trace.transition", "sketched", skippedAt, intake.OwnerActor), intake.SketchedState, "handed in")
	fixturekit.Must(t, err)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{packetOf(skipping, true, 1, 1, "ratify: no objection stands", skippedAt)}))

	_, err = repository.SetFeatureState(ctx, header(contesting, "osmia.trace.transition", "building", tick(3), intake.OwnerActor), intake.BuildingState, "sealed")
	fixturekit.Must(t, err)
	plantReport(t, repository, contesting, "resume", tick(3))
	move(contesting, trace.UnitSubject("resume"), "resume-reviewing", intake.UnitReviewing, tick(3), execution.ForemanActor)
	move(contesting, trace.UnitSubject("resume"), "resume-contested", intake.UnitContested, tick(3), execution.ReviewerActor)
	journey.RaiseFixtureContest(t, repository, contesting, "resume")
	move(contesting, trace.UnitSubject("index"), "index-implementing", intake.UnitImplementing, tick(4), execution.ForemanActor)
	move(contesting, trace.UnitSubject("index"), "index-contested", intake.UnitContested, tick(4), intake.MasonActor)
	journey.RaiseFixtureContest(t, repository, contesting, "index")

	presented, apiErr := f.S.DeliveryPresentation(ctx, string(assembled))
	if apiErr != nil {
		t.Fatal(apiErr)
	}

	sealed, sealDoc, _, err := seal.Latest(repository, assembled)
	fixturekit.Must(t, err)
	_, err = repository.FileBudgetAmendment(ctx, assembled, trace.AmendmentRequest{Citations: []string{"spec#1"}, Change: "Raise the per-unit budget.", Reason: "The upload unit needs more turns.", Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}, tick(5))
	fixturekit.Must(t, err)
	amendmentPacket := trace.Document{Header: header(assembled, "osmia.trace.document", "amendment-1-presented-packet", tick(5), intake.ShedActor), Path: intake.AmendmentPacketPath("1"), Content: `{"round":1,"recommendation":"approve: no objection stands"}` + "\n"}
	move(assembled, intake.AmendmentSubject("1"), "amendment-1-presented", intake.AmendmentPresented, tick(5), intake.ShedActor, amendmentPacket)

	body := func(kv ...any) map[string]any {
		out := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	answer := func(path string, kv ...any) api.InboxAnswer {
		return api.InboxAnswer{Method: "POST", Path: "/v1/" + path, Body: body(kv...)}
	}
	reviewed := intakefixture.StreamDocuments(t, repository, assembled, delivery.FinalReportDocument)[0]
	deliveryEntry := api.InboxEntry{Kind: api.InboxDelivery, Workstream: assembled, Revision: 1, OpenedAt: reviewed.At, Options: []string{"approve"}, Asked: []api.InboxQuestion{},
		Question: fmt.Sprintf("Mark ready for review: Resumable uploads? Final review 1 of commit %s found nothing blocking. No check result is recorded for this review. No brief was recorded for this review.", report.Commit), Blocked: "Marking the pull request ready for review.",
		Answer: answer("delivery/"+string(assembled), "review", 1, "review_revision", 1, "commit", report.Commit, "draft_hash", presented.DraftHash)}
	escalation := api.InboxEntry{Kind: api.InboxEscalation, Workstream: escalating, Number: 1, Batch: "escalation_1", Question: "Where should state live?", Blocked: "The unit.", Options: []string{"Files", "A database"},
		Recommendation: "In files.", QuickReply: "In files.", OpenedAt: tick(1), Asked: []api.InboxQuestion{{ID: "1", AskedBy: fixturekit.DemoAgent, Question: "Where does state live?"}}, Answer: answer("inbox/1", "project", string(repository.Project()))}
	ratification := api.InboxEntry{Kind: api.InboxRatification, Workstream: shedding, Revision: 2, OpenedAt: tick(2), Options: []string{"ratify"}, Asked: []api.InboxQuestion{},
		Question: "Ratify spec.md revision 1 and plan.json revision 1? Debate ended after round 1: no objection stands", Blocked: "Sealing the spec and plan, and building the workstream.",
		Recommendation: "ratify: nothing blocks, and 1 objection stands as advice on the record", Answer: answer("ratify/"+string(shedding), "spec", 1, "plan", 1)}
	skipped := api.InboxEntry{Kind: api.InboxRatification, Workstream: skipping, Revision: 1, OpenedAt: skippedAt, Options: []string{"ratify"}, Asked: []api.InboxQuestion{},
		Question: "Ratify spec.md revision 1 and plan.json revision 1? Debate was skipped: no objection stands", Blocked: "Sealing the spec and plan, and building the workstream.",
		Recommendation: "ratify: no objection stands", Answer: answer("ratify/"+string(skipping), "spec", 1, "plan", 1)}
	failedTurn := api.InboxEntry{Kind: api.InboxContested, Workstream: contesting, Unit: "resume", OpenedAt: tick(3), Options: []string{"review"}, Asked: []api.InboxQuestion{},
		Question: "Unit resume is contested: moved to contested", Blocked: "Unit resume.", Answer: answer("contested/" + string(contesting) + "/resume")}
	gaveUp := api.InboxEntry{Kind: api.InboxContested, Workstream: contesting, Unit: "index", OpenedAt: tick(4), Options: []string{"revise"}, Asked: []api.InboxQuestion{},
		Question: "Unit index is contested: moved to contested", Blocked: "Unit index.", Answer: answer("contested/" + string(contesting) + "/index")}
	amendment := api.InboxEntry{Kind: api.InboxAmendment, Workstream: assembled, Amendment: "1", Revision: 1, OpenedAt: tick(5), Options: []string{intake.AmendmentApprove, intake.AmendmentReject}, Asked: []api.InboxQuestion{},
		Question: "Amend the sealed spec and plan after debate round 1? Change: Raise the per-unit budget. Reason: The upload unit needs more turns.", Blocked: "The sealed spec and plan stay in force until the amendment is decided.",
		Recommendation: "approve: no objection stands", Answer: answer("amendment/"+string(assembled)+"/1", "packet", 1)}
	expect := func(step string, want ...api.InboxEntry) {
		t.Helper()
		got, apiErr := f.S.Inbox(ctx)
		if apiErr != nil {
			t.Fatalf("%s: %v", step, apiErr)
		}
		if want == nil {
			want = []api.InboxEntry{}
		}
		for i := range want {
			want[i].Project = repository.Project()
		}
		if !reflect.DeepEqual(got.Entries, want) {
			t.Fatalf("%s: inbox\n%+v\nwant\n%+v", step, got.Entries, want)
		}
	}
	expect("every kind open", escalation, ratification, skipped, failedTurn, gaveUp, amendment)

	if _, apiErr := f.S.Answer(ctx, "1", api.AnswerRequest{Text: "In files."}); apiErr != nil {
		t.Fatal(apiErr)
	}
	expect("escalation answered", ratification, skipped, failedTurn, gaveUp, amendment)
	if _, apiErr := f.S.RuleContested(ctx, string(contesting), "resume", api.ContestedRulingRequest{Decision: "review", Note: "Review it again."}); apiErr != nil {
		t.Fatal(apiErr)
	}
	expect("contest ruled", ratification, skipped, gaveUp, amendment)
	if _, apiErr := f.S.DecideAmendment(ctx, string(assembled), "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentReject, Packet: 1}); apiErr != nil {
		t.Fatal(apiErr)
	}
	expect("amendment rejected", deliveryEntry, ratification, skipped, gaveUp)
	if _, apiErr := f.S.ApproveDelivery(ctx, string(assembled), api.DeliveryDecision{Review: 1, ReviewRevision: 1, Commit: report.Commit, DraftHash: presented.DraftHash}); apiErr != nil {
		t.Fatal(apiErr)
	}
	expect("delivery approved", ratification, skipped, gaveUp)

	if _, err := (&journey.Publisher{S: f.S, Repository: repository}).Refuse(ctx, assembled, delivery.PublishInput{Approval: 1}, "the fork is gone"); err != nil {
		t.Fatal(err)
	}
	expect("publication refused", deliveryEntry, ratification, skipped, gaveUp)
	if _, apiErr := f.S.ApproveDelivery(ctx, string(assembled), api.DeliveryDecision{Review: 1, ReviewRevision: 1, Commit: report.Commit, DraftHash: presented.DraftHash}); apiErr != nil {
		t.Fatal(apiErr)
	}
	expect("delivery approved again", ratification, skipped, gaveUp)

	recordReport := func(revision int, when time.Time) {
		t.Helper()
		content, err := json.Marshal(report)
		fixturekit.Must(t, err)
		h := header(assembled, "osmia.trace.document", delivery.FinalReportDocument, when, delivery.FinalReviewActor)
		h.Revision, h.Cause = revision, "final-review"
		fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: h, Path: delivery.FinalReportPath, Content: string(content)}}))
	}
	recordReport(2, tick(6))
	deliveryEntry.Revision, deliveryEntry.OpenedAt = 2, tick(6)
	deliveryEntry.Answer = answer("delivery/"+string(assembled), "review", 1, "review_revision", 2, "commit", report.Commit, "draft_hash", presented.DraftHash)
	expect("final report revised", ratification, skipped, gaveUp, deliveryEntry)

	report.Criteria[1].Evidence, report.Criteria[1].Gap = "", "No duplicate test"
	recordReport(3, tick(6))
	deliveryEntry.Revision = 3
	deliveryEntry.Question = fmt.Sprintf("Mark ready for review: Resumable uploads as it is? Final review 1 of commit %s has blocking findings: spec#2: No duplicate test. Approving accepts them as they are; otherwise the work that resolves them goes through a plan revision, and a new final review follows. No check result is recorded for this review. No brief was recorded for this review.", report.Commit)
	deliveryEntry.Answer = answer("delivery/"+string(assembled), "review", 1, "review_revision", 3, "commit", report.Commit, "draft_hash", presented.DraftHash, "accept_blocking", true)
	expect("final report with something blocking", ratification, skipped, gaveUp, deliveryEntry)
	if _, apiErr := f.S.ApproveDelivery(ctx, string(assembled), api.DeliveryDecision{Review: 1, ReviewRevision: 3, Commit: report.Commit, DraftHash: presented.DraftHash, AcceptBlocking: true}); apiErr != nil {
		t.Fatal(apiErr)
	}
	expect("blocking finding accepted", ratification, skipped, gaveUp)

	move(shedding, intake.ShedSubject, "shed-round-2", "round-2", tick(7), intake.ShedActor)
	expect("debate resumed", skipped, gaveUp)
	move(shedding, intake.ShedSubject, "shed-concluded-2", "concluded-2", tick(8), intake.ShedActor)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{packet(2, 1, "ratify: no objection stands", tick(8))}))
	ratification.Revision, ratification.OpenedAt, ratification.Recommendation = 1, tick(8), "ratify: no objection stands"
	ratification.Question = "Ratify spec.md revision 1 and plan.json revision 2? Debate ended after round 2: no objection stands"
	ratification.Answer = answer("ratify/"+string(shedding), "spec", 1, "plan", 2)
	expect("second round concluded", skipped, gaveUp, ratification)
	record, err := shed.EncodeRatification(shed.Ratify(2, shed.Pin{Spec: 1, Plan: 2}, []shed.Entry{}))
	fixturekit.Must(t, err)
	ratified := header(shedding, "osmia.trace.document", shed.RatificationDocumentID(2), tick(9), intake.OwnerActor)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: ratified, Path: shed.RatificationPath(2), Content: string(record)}}))
	expect("packet ratified", skipped, gaveUp)
	record, err = shed.EncodeRatification(shed.Ratify(1, shed.Pin{Spec: 1, Plan: 1}, []shed.Entry{}))
	fixturekit.Must(t, err)
	ratified = header(skipping, "osmia.trace.document", shed.RatificationDocumentID(1), tick(9), intake.OwnerActor)
	fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: ratified, Path: shed.RatificationPath(1), Content: string(record)}}))
	expect("skipped packet ratified", gaveUp)

	_, err = repository.SetFeatureState(ctx, header(contesting, "osmia.trace.transition", intake.AbandonTransition, tick(10), intake.OwnerActor), intake.AbandonedState, "gone")
	fixturekit.Must(t, err)
	expect("workstream abandoned")
}
