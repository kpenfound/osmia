package service

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// apiError checks that err is the service's error with the code and message.
func apiError(t *testing.T, err error, code Code, message string) {
	t.Helper()
	var api *APIError
	if !errors.As(err, &api) || api.Code != code || api.Message != message {
		t.Fatalf("want %s %q, got %v", code, message, err)
	}
}

// Fake askers in two workstreams park on escalated questions. The owner reads
// one inbox, answers the batch once, and the service restarts after the
// ruling, after the relay and with the answer turns queued but not run. Each
// asker resumes exactly once on the thread that asked.
func TestOwnerRulingResumesTheAskersAcrossRestarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newQuestionFixture(t, "qi-", "", "# Charter\n\n1. Keep state in files under the root.\n", []questionAsker{
		{stream, demoAgent, demoThread, "mason", "build"}, {stream, "agent_reviewer", "thread_reviewer", "reviewer", "review"}, {quiet, "agent_other", "thread_other", "mason", "build_other"},
	})
	asker := func(question string) demoTurn {
		return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := f.use(ctx, tools, req.Name, "ask", map[string]any{"question": question}); err != nil {
				return nil, err
			}
			return questionResult(req, "session-"+req.Name, "Asked"), nil
		}
	}
	f.engine.turns["build"] = asker("Where does state live?")
	f.engine.turns["review"] = asker("Is the log format fixed?")
	f.engine.turns["build_other"] = asker("May I add a dependency?")
	// The answer arrives after the asking turn, on the same session's thread.
	resumed := func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if names, err := toolNames(ctx, tools); err != nil || !slices.Equal(names, []string{"ask", "file_read"}) {
			f.problem("%s tools %v: %v", req.Name, names, err)
		}
		if !strings.Contains(req.Prompt, "Work: ") {
			f.problem("%s does not continue the asking thread:\n%s", req.Name, req.Prompt)
		}
		f.results["resumed"] = append(f.results["resumed"], req.Prompt[strings.Index(req.Prompt, "The owner ruled"):])
		return questionResult(req, "session-"+req.Name, "Built"), nil
	}
	f.engine.turns["answer_1"], f.engine.turns["answer_2"] = resumed, resumed

	relayed := make(chan struct{})
	notice := "- workstreams/" + string(stream) + "/questions/1/rulings.jsonl (record 1 revision 2, workstream " + string(stream) + ")\n  notice: State and the log format are part of the contract.\n"
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		f.mu.Lock()
		switch {
		// The prompt replays the thread's earlier turns, so the newest event is
		// matched first.
		case strings.Contains(req.Prompt, ", escalation_1 (questions 1, 2): Both are part of the contract."):
			if strings.Contains(req.SystemPrompt, "  notice: ") {
				f.problem("a notice before any notify ruling:\n%s", req.SystemPrompt)
			}
			for _, args := range []map[string]any{
				{"question": "2", "text": "State and the log format are part of the contract.", "scope": "notify"},
				{"question": "1", "text": "Again.", "scope": "local"},
			} {
				if err := f.use(ctx, tools, "relay", "relay_ruling", args); err != nil {
					f.problem("relay_ruling: %v", err)
					f.mu.Unlock()
					return nil, err
				}
			}
			f.mu.Unlock()
			// The service stops before any pass can deliver the relayed ruling.
			close(relayed)
			<-ctx.Done()
			return nil, ctx.Err()
		case strings.Contains(req.Prompt, ", escalation_1 (questions 1): No new dependencies."):
			defer f.mu.Unlock()
			// The other workstream's notify ruling is part of this context.
			if !strings.Contains(req.SystemPrompt, "## Notices\n"+notice) {
				f.problem("event turn system prompt lacks the notice:\n%s", req.SystemPrompt)
			}
			return questionResult(req, "session-chief", "Relayed"), f.use(ctx, tools, "relay-local", "relay_ruling", map[string]any{"question": "1", "text": "Add no dependency.", "scope": "local"})
		case strings.Contains(req.Prompt, "Where does state live?") && strings.Contains(req.Prompt, "Is the log format fixed?") && strings.Contains(req.Prompt, "is open"):
			defer f.mu.Unlock()
			return questionResult(req, "session-chief", "Escalated"), f.use(ctx, tools, "escalate", "escalate", map[string]any{"questions": []string{"1", "2"},
				"rephrasing": "Are state files and the log format part of the contract?", "blocked": "The upload unit and its review.", "options": []string{"Both fixed", "Both free"}, "recommendation": "Both fixed."})
		case strings.Contains(req.Prompt, "is open, asked by the mason: May I add a dependency?"):
			defer f.mu.Unlock()
			return questionResult(req, "session-chief", "Escalated"), f.use(ctx, tools, "escalate", "escalate", map[string]any{"questions": []string{"1"},
				"rephrasing": "May the index unit add a dependency?", "blocked": "The index unit.", "options": []string{}, "recommendation": "No."})
		}
		f.problem("unexpected chief-of-staff turn:\n%s", req.Prompt)
		f.mu.Unlock()
		return questionResult(req, "session-chief", "Nothing to do"), nil
	}

	states := func(r *trace.Repository, ws config.WorkstreamID) map[string]string {
		t.Helper()
		list, err := r.Questions(ws)
		must(t, err)
		out := map[string]string{}
		for _, q := range list {
			out[q.Asked.ID] = q.State
		}
		return out
	}
	thread := func(r *trace.Repository, ws config.WorkstreamID, agent string) trace.Thread {
		t.Helper()
		th, err := r.Thread(ws, agent)
		must(t, err)
		return th
	}
	turnsOf := func(r *trace.Repository, ws config.WorkstreamID, agent string) (out []string) {
		for _, q := range thread(r, ws, agent).Turns {
			out = append(out, q.Request.TurnID)
		}
		return out
	}
	runs := func() map[string]int {
		f.engine.mu.Lock()
		defer f.engine.mu.Unlock()
		out := map[string]int{}
		for _, name := range f.engine.runs {
			if strings.HasPrefix(name, "events_") {
				name = "events"
			}
			out[name]++
		}
		return out
	}
	lifetime := func() (*Service, *Client, *trace.Repository) {
		t.Helper()
		s, err := Start(ctx, f.opts)
		must(t, err)
		return s, NewClient(s.Socket()), <-f.lives
	}
	stop := func(s *Service, c *Client) {
		t.Helper()
		c.Close()
		must(t, s.Close())
	}

	// First lifetime: the askers ask and park, both chiefs of staff escalate,
	// and the owner reads the inbox and answers the batch.
	s, c, repo := lifetime()
	f.settle(t, s)
	f.clock.Advance(time.Minute)
	f.settle(t, s)
	inbox, err := c.Inbox(ctx)
	must(t, err)
	if len(inbox.Entries) != 2 || inbox.Entries[0].Number != 1 || inbox.Entries[1].Number != 2 {
		t.Fatalf("inbox: %+v", inbox)
	}
	batch, single := inbox.Entries[0], inbox.Entries[1]
	if batch.Workstream != stream {
		batch, single = single, batch
	}
	wantBatch := InboxEntry{Number: batch.Number, Workstream: stream, Batch: "escalation_1", Question: "Are state files and the log format part of the contract?", Blocked: "The upload unit and its review.",
		Options: []string{"Both fixed", "Both free"}, Recommendation: "Both fixed.", EscalatedAt: f.clock.Now(),
		Asked: []InboxQuestion{{ID: "1", AskedBy: demoAgent, Question: "Where does state live?"}, {ID: "2", AskedBy: "agent_reviewer", Question: "Is the log format fixed?"}}}
	// The two askers run in one pass, in either order.
	if batch.Asked[0].Question != "Where does state live?" {
		wantBatch.Asked[0].Question, wantBatch.Asked[1].Question = wantBatch.Asked[1].Question, wantBatch.Asked[0].Question
		wantBatch.Asked[0].AskedBy, wantBatch.Asked[1].AskedBy = wantBatch.Asked[1].AskedBy, wantBatch.Asked[0].AskedBy
	}
	if !reflect.DeepEqual(batch, wantBatch) {
		t.Fatalf("batch entry:\n%+v\nwant\n%+v", batch, wantBatch)
	}
	if single.Workstream != quiet || single.Batch != "escalation_1" || single.Question != "May the index unit add a dependency?" || single.Options == nil || len(single.Options) != 0 || len(single.Asked) != 1 || single.Asked[0].AskedBy != "agent_other" {
		t.Fatalf("single entry: %+v", single)
	}

	traceDir, err := f.cfg.Root.ProjectTrace(project)
	must(t, err)
	head := demoGit(t, "", "-C", traceDir, "rev-parse", "HEAD")
	_, err = c.Answer(ctx, 9, "Both are part of the contract.")
	apiError(t, err, Validation, "there is no inbox entry 9; list the entries with osmia inbox")
	_, err = c.Answer(ctx, batch.Number, " \n")
	apiError(t, err, Validation, "text must not be empty")
	for _, number := range []string{"0", "-1", "01", "x", "1.5"} {
		apiError(t, c.Do(ctx, "POST", Prefix+"/inbox/"+number, AnswerRequest{Text: "x"}, new(AnswerResponse)), Validation, "inbox entry must be a number from osmia inbox")
	}
	if got := demoGit(t, "", "-C", traceDir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("a refused answer committed: %s, was %s", got, head)
	}
	answered, err := c.Answer(ctx, batch.Number, "Both are part of the contract.")
	must(t, err)
	if !reflect.DeepEqual(answered, AnswerResponse{Number: batch.Number, Workstream: stream, Batch: "escalation_1", Questions: []string{"1", "2"}, Ruling: "Both are part of the contract.", At: f.clock.Now()}) {
		t.Fatalf("answer: %+v", answered)
	}
	_, err = c.Answer(ctx, batch.Number, "Neither is.")
	apiError(t, err, Conflict, "inbox entry "+strconv.Itoa(batch.Number)+" is already answered")
	inbox, err = c.Inbox(ctx)
	must(t, err)
	if len(inbox.Entries) != 1 || !reflect.DeepEqual(inbox.Entries[0], single) {
		t.Fatalf("inbox after the answer: %+v", inbox)
	}
	list, err := repo.Questions(stream)
	must(t, err)
	for _, q := range list {
		if r := q.Ruling; q.State != trace.QuestionRuled || r == nil || r.OwnerResponse != "Both are part of the contract." || r.ReturnedAnswer != "" || r.Actor != ownerActor || r.Decision != trace.DecisionRuling {
			t.Fatalf("ruled question %s: %+v", q.Asked.ID, q)
		}
	}
	// The turns of this workstream's askers are held from here on.
	target := runtime.Target{Scope: "workstream", Project: project, Workstream: stream}
	mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Source: "operator"})
	started := map[string]int{"build": 1, "review": 1, "build_other": 1, "events": 2}
	if got := runs(); !reflect.DeepEqual(got, started) {
		t.Fatalf("first lifetime runs: %v", got)
	}
	stop(s, c)

	// Second lifetime, after the ruling: the chief of staff hears of it once
	// the event window closes, relays it, and the service stops before the
	// relayed ruling is delivered.
	s, c, repo = lifetime()
	f.settle(t, s)
	if got := runs(); !reflect.DeepEqual(got, started) {
		t.Fatalf("restart after the ruling ran: %v", got)
	}
	f.clock.Advance(time.Minute)
	deadline := time.After(demoTimeout)
	for waiting := true; waiting; {
		select {
		case f.ticks <- f.clock.Now():
		case <-relayed:
			waiting = false
		case <-deadline:
			s.Close()
			t.Fatalf("chief of staff did not relay the ruling; runs %v, fake agents saw %v", runs(), f.problems)
		}
	}
	if got := states(repo, stream); !reflect.DeepEqual(got, map[string]string{"1": trace.QuestionAnswered, "2": trace.QuestionAnswered}) {
		t.Fatalf("questions after the relay: %v", got)
	}
	if got := turnsOf(repo, stream, demoAgent); len(got) != 1 {
		t.Fatalf("ruling delivered inside the chief-of-staff turn: %v", got)
	}
	stop(s, c)

	// Third lifetime, after the relay: each asker's thread gets its answer
	// turn, which the pause holds.
	s, c, repo = lifetime()
	f.settle(t, s)
	queued := func(r *trace.Repository) {
		t.Helper()
		mason, reviewer := thread(r, stream, demoAgent), thread(r, stream, "agent_reviewer")
		if len(mason.Turns) != 2 || len(reviewer.Turns) != 2 || mason.Turns[1].Claim != nil || reviewer.Turns[1].Claim != nil {
			t.Fatalf("held answer turns: mason %v, reviewer %v", turnsOf(r, stream, demoAgent), turnsOf(r, stream, "agent_reviewer"))
		}
	}
	queued(repo)
	stop(s, c)

	// Fourth lifetime, before delivery completes: the queued turns are not
	// queued again, and run once the pause is cleared. Then the owner answers
	// the other workstream's entry, which its chief of staff relays locally.
	s, c, repo = lifetime()
	f.settle(t, s)
	queued(repo)
	if got := runs(); got["answer_1"] != 0 || got["answer_2"] != 0 {
		t.Fatalf("held answer turns ran: %v", got)
	}
	mutation(t, c, "DELETE", "pause", ClearPauseRequest(target))
	f.settle(t, s)
	for agent, thread := range map[string]string{demoAgent: demoThread, "agent_reviewer": "thread_reviewer"} {
		th, err := repo.Thread(stream, agent)
		must(t, err)
		if len(th.Turns) != 2 || th.Parked() || th.Turns[1].Request.ThreadID != thread || th.Turns[1].Response == nil || th.Turns[1].Status() != "idle" {
			t.Fatalf("%s after the ruling: parked %v, turns %+v", agent, th.Parked(), th.Turns)
		}
	}
	_, err = c.Answer(ctx, single.Number, "No new dependencies.")
	must(t, err)
	f.clock.Advance(time.Minute)
	f.settle(t, s)
	f.settle(t, s)
	if th := thread(repo, quiet, "agent_other"); len(th.Turns) != 2 || th.Parked() || th.Turns[1].Request.ThreadID != "thread_other" {
		t.Fatalf("other workstream's asker: parked %v, turns %v", th.Parked(), turnsOf(repo, quiet, "agent_other"))
	}
	inbox, err = c.Inbox(ctx)
	must(t, err)
	if len(inbox.Entries) != 0 || inbox.Entries == nil {
		t.Fatalf("inbox after every ruling: %+v", inbox)
	}
	// Only the notify ruling is a notice, in both workstreams' bundles.
	for _, ws := range []config.WorkstreamID{stream, quiet} {
		b, err := s.Context().Assemble(ctx, project, bundle.Scope{Workstream: ws})
		must(t, err)
		if len(b.Notices) != 1 || !strings.HasSuffix(b.Render(), "## Notices\n"+notice) {
			t.Fatalf("notices of %s: %+v", ws, b.Notices)
		}
	}
	stop(s, c)

	// Fifth lifetime: everything is delivered, and nothing runs again.
	s, c, repo = lifetime()
	f.clock.Advance(24 * time.Hour)
	f.settle(t, s)
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"build": 1, "review": 1, "build_other": 1, "events": 4, "answer_1": 2, "answer_2": 1}) {
		t.Fatalf("runs: %v", got)
	}
	if got := turnsOf(repo, stream, "agent_reviewer"); len(got) != 2 {
		t.Fatalf("reviewer turns: %v", got)
	}
	stop(s, c)

	f.mu.Lock()
	defer f.mu.Unlock()
	slices.Sort(f.results["resumed"])
	ruled := func(id, question, answer string) string {
		return "The owner ruled on your question " + id + ". The chief of staff relays the ruling.\n\nYou asked:\n" + question + "\n\nAnswer:\n" + answer + "\n"
	}
	wantResumed := []string{ruled("1", wantBatch.Asked[0].Question, "State and the log format are part of the contract."), ruled("1", "May I add a dependency?", "Add no dependency."),
		ruled("2", wantBatch.Asked[1].Question, "State and the log format are part of the contract.")}
	slices.Sort(wantResumed)
	if !slices.Equal(f.results["resumed"], wantResumed) {
		t.Fatalf("resumed prompts:\n%q\nwant\n%q", f.results["resumed"], wantResumed)
	}
	if got, want := f.results["relay"], []string{
		`{"recorded":true,"questions":["1","2"],"scope":"notify","next":"The ruling is delivered to each asker as its next turn."}`,
		`{"recorded":false,"reason":"question 1 is already answered"}`,
	}; !slices.Equal(got, want) {
		t.Fatalf("relay results: %v", got)
	}
	if got := f.results["escalate"]; !slices.Equal(got, []string{`{"recorded":true,"batch":"escalation_1","questions":["1","2"]}`, `{"recorded":true,"batch":"escalation_1","questions":["1"]}`}) {
		t.Fatalf("escalation results: %v", got)
	}
	if got := f.results["relay-local"]; !slices.Equal(got, []string{`{"recorded":true,"questions":["1"],"scope":"local","next":"The ruling is delivered to each asker as its next turn."}`}) {
		t.Fatalf("local relay result: %v", got)
	}
	if len(f.problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(f.problems, "\n"))
	}
}

// The inbox leaves out an abandoned workstream's escalation, which takes no
// ruling, and a service without a trace or without a project has an empty
// inbox.
func TestInboxLeavesOutAbandonedWorkstreams(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "qj-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	clock := &fixedClock{now: demoStart}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, clock.Now(), ownerActor)
	must(t, err)
	defer repo.Close()
	for _, ws := range []config.WorkstreamID{stream, quiet} {
		must(t, repo.CreateWorkstream(ctx, ws, clock.Now(), ownerActor))
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: demoAgent, Project: project, Workstream: ws, At: clock.Now(), Actor: ownerActor, Cause: "workstream_created"}, Role: demoRole, ThreadID: demoThread}
		must(t, repo.CreateThread(ctx, identity))
		_, err := repo.Ask(ctx, demoAgent, claimTurn(t, repo, home, ws, demoAgent, demoThread, "build", clock.Now()), "Where does state live?", clock.Now())
		must(t, err)
		_, err = repo.EscalateQuestions(ctx, trace.ChiefOfStaff, claimTurn(t, repo, home, ws, trace.ChiefOfStaff, trace.ChiefOfStaff, "events", clock.Now()),
			trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "Where should state live?", Blocked: "The unit.", Recommendation: "In files."}, clock.Now())
		must(t, err)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: abandonTransition, Revision: 1, Project: project, Workstream: stream, At: clock.Now(), Actor: ownerActor, Cause: abandonTransition}
	_, err = repo.SetFeatureState(ctx, h, AbandonedState, "gone")
	must(t, err)

	s := &Service{cfg: cfg, active: &activeProject{repository: repo}, options: Options{Reconciliation: reconcile.Options{Now: clock.Now}}}
	inbox, api := s.inbox()
	if api != nil || len(inbox.Entries) != 1 || inbox.Entries[0].Number != 2 || inbox.Entries[0].Workstream != quiet {
		t.Fatalf("inbox: %+v %v", inbox, api)
	}
	_, api = s.answer(ctx, "1", AnswerRequest{Text: "In files."})
	if api == nil || api.Code != Conflict || api.Message != "inbox entry 1 belongs to abandoned workstream "+string(stream)+" and takes no ruling" {
		t.Fatalf("answer to an abandoned workstream's entry: %v", api)
	}
	if state, err := repo.Workflow(stream, trace.QuestionSubject("1")); err != nil || state.Value != trace.QuestionEscalated {
		t.Fatalf("abandoned workstream's question: %+v %v", state, err)
	}
	if out, api := s.answer(ctx, "2", AnswerRequest{Text: "In files."}); api != nil || out.Workstream != quiet {
		t.Fatalf("answer: %+v %v", out, api)
	}

	idle := &Service{cfg: cfg}
	if inbox, api := idle.inbox(); api != nil || inbox.Entries == nil || len(inbox.Entries) != 0 {
		t.Fatalf("inbox without a trace: %+v %v", inbox, api)
	}
	if _, api := idle.answer(ctx, "1", AnswerRequest{Text: "In files."}); api == nil || api.Code != Validation || api.Message != "there is no inbox entry 1; list the entries with osmia inbox" {
		t.Fatalf("answer without a trace: %v", api)
	}

	empty, _ := projectFixture(t)
	_, c := start(t, empty)
	if inbox, err := c.Inbox(ctx); err != nil || inbox.Entries == nil || len(inbox.Entries) != 0 {
		t.Fatalf("inbox without a project: %+v %v", inbox, err)
	}
	_, err = c.Answer(ctx, 1, "In files.")
	apiError(t, err, NoProject, "no project is configured; add one with osmia project add")
}
