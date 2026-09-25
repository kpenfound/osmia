package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/envelope"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
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

func TestQuickReplyEligibility(t *testing.T) {
	for _, word := range []string{"push", "merge", "deliver", "abandon", "force", "delete", "rebase", "overrule", "deploy", "ratify", "veto", "revert", "reset", "discard", "PUSHES", "merged", "ratified", "vetoing", "abandoned", "force-push", "Merge,please"} {
		t.Run(word, func(t *testing.T) {
			entry := trace.InboxEntry{State: trace.QuestionEscalated, Recommendation: "Please " + word + " this.", Questions: []trace.QuestionState{{}}}
			if got := inboxView(entry).QuickReply; got != "" {
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
		if got := inboxView(entry).QuickReply; got != want {
			t.Fatalf("quick reply for %q: %q, want %q", recommendation, got, want)
		}
	}
	for _, decision := range []string{"ratification", "contested-unit", "amendment-decision", "delivery-approval"} {
		entry := trace.InboxEntry{State: decision, Recommendation: "Proceed with the plan.", Questions: []trace.QuestionState{{}}}
		if got := inboxView(entry).QuickReply; got != "" {
			t.Fatalf("%s quick reply: %q", decision, got)
		}
	}
	if got := inboxView(trace.InboxEntry{State: trace.QuestionEscalated, Recommendation: "Proceed with the plan."}).QuickReply; got != "" {
		t.Fatalf("entry without questions has quick reply %q", got)
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
	section, sectionErr := envelope.Render(envelope.Section{Name: "owner_response", Text: "Both are part of the contract."}, envelope.Section{Name: "returned_answer", Text: "State and the log format are part of the contract."})
	if sectionErr != nil {
		t.Fatal(sectionErr)
	}
	notice := "- workstreams/" + string(stream) + "/questions/1/rulings.jsonl (record 1 revision 2, workstream " + string(stream) + ")\n" + section
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		f.mu.Lock()
		switch {
		// The prompt replays the thread's earlier turns, so the newest event is
		// matched first.
		case strings.Contains(req.Prompt, ", escalation_1 (questions 1, 2): Both are part of the contract."):
			select {
			case <-relayed:
				// The cancelled relaying turn left its event unacknowledged, so
				// a later lifetime delivers it again.
				f.mu.Unlock()
				return questionResult(req, "session-chief", "Already relayed"), nil
			default:
			}
			if strings.Contains(req.SystemPrompt, "<<< osmia:owner_response") {
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
	wantBatch := InboxEntry{Kind: InboxEscalation, Number: batch.Number, Workstream: stream, Batch: "escalation_1", Question: "Are state files and the log format part of the contract?", Blocked: "The upload unit and its review.",
		Options: []string{"Both fixed", "Both free"}, Recommendation: "Both fixed.", QuickReply: "Both fixed.", OpenedAt: f.clock.Now(), Answer: InboxAnswer{Method: "POST", Path: "/v1/inbox/" + strconv.Itoa(batch.Number), Body: map[string]any{}},
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
	if got, err := c.Status(ctx, stream); err != nil || !reflect.DeepEqual(got.Gates, []trace.OwnerGate{{Kind: "escalation", Reference: strconv.Itoa(batch.Number)}}) {
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
	if got, err := c.Status(ctx, stream); err != nil || len(got.Gates) != 0 {
		t.Fatalf("ruled status gates: %+v %v", got.Gates, err)
	}
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
	mutation(t, c, "PUT", "pause", PauseRequest{Target: target, Mode: "soft", Source: "owner"})
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

	// Fifth lifetime: everything is delivered, and nothing runs again. The
	// cancelled relaying turn's events were delivered once more.
	s, c, repo = lifetime()
	f.clock.Advance(24 * time.Hour)
	f.settle(t, s)
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"build": 1, "review": 1, "build_other": 1, "events": 5, "answer_1": 2, "answer_2": 1}) {
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
		ownerWords := "Both are part of the contract."
		if answer == "Add no dependency." {
			ownerWords = "No new dependencies."
		}
		return questions.Prompt(trace.Question{Header: trace.Header{ID: id}, Question: question}, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: ownerWords, ReturnedAnswer: answer})
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
	inbox, api := s.inbox(ctx)
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
	if inbox, api := idle.inbox(ctx); api != nil || inbox.Entries == nil || len(inbox.Entries) != 0 {
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

// One trace holds an open decision of every kind. The inbox lists each once,
// oldest first, with the endpoint and identity that answer it; answering
// through those endpoints, superseding what was presented and abandoning a
// workstream each take the entry out.
func TestInboxListsEveryOpenDecision(t *testing.T) {
	t.Parallel()
	f, assembled, repository, report := deliveryFixture(t)
	defer repository.Close()
	ctx := context.Background()
	p := repository.Project()
	at := time.Now().UTC().Add(time.Minute)
	tick := func(n int) time.Time { return at.Add(time.Duration(n) * time.Second) }
	if state, err := repository.Workflow(assembled, trace.FeatureSubject); err != nil || state.Value != AssembledState {
		t.Fatalf("fixture workstream: %+v %v", state, err)
	}
	escalating, shedding, contesting := config.WorkstreamID("w_"+strings.Repeat("1", 32)), config.WorkstreamID("w_"+strings.Repeat("2", 32)), config.WorkstreamID("w_"+strings.Repeat("3", 32))
	for _, ws := range []config.WorkstreamID{escalating, shedding, contesting} {
		must(t, repository.CreateWorkstream(ctx, ws, at, ownerActor))
	}
	header := func(ws config.WorkstreamID, schema, id string, when time.Time, actor trace.Actor) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: p, Workstream: ws, At: when, Actor: actor, Cause: "test"}
	}
	move := func(ws config.WorkstreamID, subject, id, to string, when time.Time, actor trace.Actor, docs ...trace.Document) {
		t.Helper()
		state, err := repository.Workflow(ws, subject)
		must(t, err)
		h := header(ws, "osmia.trace.transition", id, when, actor)
		tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: to, Reason: "moved to " + to}}
		if len(docs) > 0 {
			_, err = repository.RecordDocumentsWith(ctx, docs, tx)
		} else {
			_, err = repository.Transact(ctx, tx)
		}
		must(t, err)
	}
	claim := func(ws config.WorkstreamID, agent, thread, turn string) coreadapter.Scope {
		t.Helper()
		h := header(ws, "osmia.trace.turn-request", "request_"+turn, at, ownerActor)
		_, err := repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: agent, ThreadID: thread, TurnID: turn, Profile: coreadapter.Profile{Name: "other", Backend: "codex", Model: "other"}, Prompt: "Work"})
		must(t, err)
		_, err = repository.ClaimTurn(ctx, ws, agent, "token_"+turn, filepath.Join(t.TempDir(), turn), at)
		must(t, err)
		th, err := repository.Thread(ws, agent)
		must(t, err)
		return coreadapter.Scope{Project: string(p), Workstream: string(ws), Thread: thread, Turn: turn, Role: th.Identity.Role}
	}

	// An escalated question.
	must(t, repository.CreateThread(ctx, trace.Agent{Header: header(escalating, "osmia.trace.agent", demoAgent, at, ownerActor), Role: demoRole, ThreadID: demoThread}))
	_, err := repository.Ask(ctx, demoAgent, claim(escalating, demoAgent, demoThread, "build"), "Where does state live?", tick(1))
	must(t, err)
	_, err = repository.EscalateQuestions(ctx, trace.ChiefOfStaff, claim(escalating, trace.ChiefOfStaff, trace.ChiefOfStaff, "events"),
		trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "Where should state live?", Blocked: "The unit.", Options: []string{"Files", "A database"}, Recommendation: "In files."}, tick(1))
	must(t, err)

	// A ratification packet whose first revision a second supersedes.
	_, err = repository.SetFeatureState(ctx, header(shedding, "osmia.trace.transition", "in-shed", tick(2), ownerActor), InShedState, "handed in")
	must(t, err)
	move(shedding, shedSubject, "shed-concluded-1", "concluded-1", tick(2), shedActor)
	packet := func(round, revision int, recommendation string, when time.Time) trace.Document {
		content, err := shed.EncodePacket(shed.Packet{Version: shed.Version, Round: round, Revision: shed.Pin{Spec: 1, Plan: round}, Conclusion: "no objection stands", Dissent: []shed.Entry{}, Recommendation: recommendation})
		must(t, err)
		h := header(shedding, "osmia.trace.document", shed.PacketDocumentID(round), when, shedActor)
		h.Revision, h.Cause = revision, "ratification-packet"
		return trace.Document{Header: h, Path: shed.PacketPath(round), Content: string(content)}
	}
	must(t, repository.RecordDocuments(ctx, []trace.Document{packet(1, 1, "ratify: no objection stands", tick(2))}))
	must(t, repository.RecordDocuments(ctx, []trace.Document{packet(1, 2, "ratify: nothing blocks, and 1 objection stands as advice on the record", tick(2))}))

	// Two contested units: one after a failed review turn, one a mason gave
	// up on.
	_, err = repository.SetFeatureState(ctx, header(contesting, "osmia.trace.transition", "building", tick(3), ownerActor), BuildingState, "sealed")
	must(t, err)
	move(contesting, trace.UnitSubject("resume"), "resume-reviewing", UnitReviewing, tick(3), foremanActor)
	move(contesting, trace.UnitSubject("resume"), "resume-contested", UnitContested, tick(3), reviewerActor)
	move(contesting, trace.UnitSubject("index"), "index-implementing", UnitImplementing, tick(4), foremanActor)
	move(contesting, trace.UnitSubject("index"), "index-contested", UnitContested, tick(4), masonActor)

	// A presented amendment in the assembled workstream.
	sealed, sealDoc, _, err := seal.Latest(repository, assembled)
	must(t, err)
	_, err = repository.FileBudgetAmendment(ctx, assembled, trace.AmendmentRequest{Citations: []string{"spec#1"}, Change: "Raise the per-unit budget.", Reason: "The upload unit needs more turns.", Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}, tick(5))
	must(t, err)
	amendmentPacket := trace.Document{Header: header(assembled, "osmia.trace.document", "amendment-1-presented-packet", tick(5), shedActor), Path: amendmentPacketPath("1"), Content: `{"round":1,"recommendation":"approve: no objection stands"}` + "\n"}
	move(assembled, amendmentSubject("1"), "amendment-1-presented", amendmentPresented, tick(5), shedActor, amendmentPacket)

	presented, api := f.s.deliveryPresentation(ctx, string(assembled))
	if api != nil {
		t.Fatal(api)
	}
	body := func(kv ...any) map[string]any {
		out := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	answer := func(path string, kv ...any) InboxAnswer {
		return InboxAnswer{Method: "POST", Path: "/v1/" + path, Body: body(kv...)}
	}
	reviewed := streamDocuments(t, repository, assembled, finalReportDocument)[0]
	delivery := InboxEntry{Kind: InboxDelivery, Workstream: assembled, Revision: 1, OpenedAt: reviewed.At, Options: []string{"approve"}, Asked: []InboxQuestion{},
		Question: fmt.Sprintf("Deliver Resumable uploads? Final review 1 of commit %s shows evidence for every criterion.", report.Commit), Blocked: "Publishing the pull request.",
		Answer: answer("delivery/"+string(assembled), "review", 1, "review_revision", 1, "commit", report.Commit, "draft_hash", presented.DraftHash)}
	escalation := InboxEntry{Kind: InboxEscalation, Workstream: escalating, Number: 1, Batch: "escalation_1", Question: "Where should state live?", Blocked: "The unit.", Options: []string{"Files", "A database"},
		Recommendation: "In files.", QuickReply: "In files.", OpenedAt: tick(1), Asked: []InboxQuestion{{ID: "1", AskedBy: demoAgent, Question: "Where does state live?"}}, Answer: answer("inbox/1")}
	ratification := InboxEntry{Kind: InboxRatification, Workstream: shedding, Revision: 2, OpenedAt: tick(2), Options: []string{"ratify"}, Asked: []InboxQuestion{},
		Question: "Ratify spec.md revision 1 and plan.json revision 1? Debate ended after round 1: no objection stands", Blocked: "Sealing the spec and plan, and building the workstream.",
		Recommendation: "ratify: nothing blocks, and 1 objection stands as advice on the record", Answer: answer("ratify/"+string(shedding), "spec", 1, "plan", 1)}
	failedTurn := InboxEntry{Kind: InboxContested, Workstream: contesting, Unit: "resume", OpenedAt: tick(3), Options: []string{"review"}, Asked: []InboxQuestion{},
		Question: "Unit resume is contested: moved to contested", Blocked: "Unit resume.", Answer: answer("contested/" + string(contesting) + "/resume")}
	gaveUp := InboxEntry{Kind: InboxContested, Workstream: contesting, Unit: "index", OpenedAt: tick(4), Options: []string{"revise"}, Asked: []InboxQuestion{},
		Question: "Unit index is contested: moved to contested", Blocked: "Unit index.", Answer: answer("contested/" + string(contesting) + "/index")}
	amendment := InboxEntry{Kind: InboxAmendment, Workstream: assembled, Amendment: "1", Revision: 1, OpenedAt: tick(5), Options: []string{AmendmentApprove, AmendmentReject}, Asked: []InboxQuestion{},
		Question: "Amend the sealed spec and plan after debate round 1? Change: Raise the per-unit budget. Reason: The upload unit needs more turns.", Blocked: "The sealed spec and plan stay in force until the amendment is decided.",
		Recommendation: "approve: no objection stands", Answer: answer("amendment/"+string(assembled)+"/1", "packet", 1)}
	expect := func(step string, want ...InboxEntry) {
		t.Helper()
		got, api := f.s.inbox(ctx)
		if api != nil {
			t.Fatalf("%s: %v", step, api)
		}
		if want == nil {
			want = []InboxEntry{}
		}
		if !reflect.DeepEqual(got.Entries, want) {
			t.Fatalf("%s: inbox\n%+v\nwant\n%+v", step, got.Entries, want)
		}
	}
	expect("every kind open", delivery, escalation, ratification, failedTurn, gaveUp, amendment)

	// Answering through each kind's own endpoint takes its entry out.
	if _, api := f.s.answer(ctx, "1", AnswerRequest{Text: "In files."}); api != nil {
		t.Fatal(api)
	}
	expect("escalation answered", delivery, ratification, failedTurn, gaveUp, amendment)
	if _, api := f.s.ruleContested(ctx, string(contesting), "resume", ContestedRulingRequest{Decision: "review", Note: "Review it again."}); api != nil {
		t.Fatal(api)
	}
	expect("contest ruled", delivery, ratification, gaveUp, amendment)
	if _, api := f.s.decideAmendment(ctx, string(assembled), "1", AmendmentDecisionRequest{Decision: AmendmentReject, Packet: 1}); api != nil {
		t.Fatal(api)
	}
	expect("amendment rejected", delivery, ratification, gaveUp)
	if _, api := f.s.approveDelivery(ctx, string(assembled), DeliveryDecision{Review: 1, ReviewRevision: 1, Commit: report.Commit, DraftHash: presented.DraftHash}); api != nil {
		t.Fatal(api)
	}
	expect("delivery approved", ratification, gaveUp)
	// A refused publication asks for the owner's approval again.
	if _, err := (&publisher{s: f.s, repository: repository}).refuse(ctx, assembled, publishInput{Approval: 1}, "the fork is gone"); err != nil {
		t.Fatal(err)
	}
	expect("publication refused", delivery, ratification, gaveUp)
	// A final report with a gap cannot be approved.
	report.Criteria[1].Evidence, report.Criteria[1].Gap = "", "No duplicate test"
	content, err := json.Marshal(report)
	must(t, err)
	gap := header(assembled, "osmia.trace.document", finalReportDocument, tick(6), finalReviewActor)
	gap.Revision, gap.Cause = 2, "final-review"
	must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: gap, Path: finalReportPath, Content: string(content)}}))
	expect("final report with a gap", ratification, gaveUp)

	// Debate that resumes leaves the packet behind; the packet of the round
	// it concludes is open until the owner ratifies it.
	move(shedding, shedSubject, "shed-round-2", "round-2", tick(7), shedActor)
	expect("debate resumed", gaveUp)
	move(shedding, shedSubject, "shed-concluded-2", "concluded-2", tick(8), shedActor)
	must(t, repository.RecordDocuments(ctx, []trace.Document{packet(2, 1, "ratify: no objection stands", tick(8))}))
	ratification.Revision, ratification.OpenedAt, ratification.Recommendation = 1, tick(8), "ratify: no objection stands"
	ratification.Question = "Ratify spec.md revision 1 and plan.json revision 2? Debate ended after round 2: no objection stands"
	ratification.Answer = answer("ratify/"+string(shedding), "spec", 1, "plan", 2)
	expect("second round concluded", gaveUp, ratification)
	record, err := shed.EncodeRatification(shed.Ratify(2, shed.Pin{Spec: 1, Plan: 2}, []shed.Entry{}))
	must(t, err)
	ratified := header(shedding, "osmia.trace.document", shed.RatificationDocumentID(2), tick(9), ownerActor)
	must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: ratified, Path: shed.RatificationPath(2), Content: string(record)}}))
	expect("packet ratified", gaveUp)

	// An abandoned workstream's decisions take no answer.
	_, err = repository.SetFeatureState(ctx, header(contesting, "osmia.trace.transition", abandonTransition, tick(10), ownerActor), AbandonedState, "gone")
	must(t, err)
	expect("workstream abandoned")
}
