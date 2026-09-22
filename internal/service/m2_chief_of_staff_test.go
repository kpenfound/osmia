package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// The M2 chief-of-staff demonstration: on an onboarded project, the owner
// talks to a workstream's chief of staff, fake workers ask questions, the
// chief of staff answers one and escalates another, and the owner's ruling
// reaches the asker across a service restart. See docs/m2-chief-of-staff.md.

// chiefDemoRoles runs every role of the demonstration in a container, the
// only sandbox the fake engine's boundary check accepts, with one mason slot.
const chiefDemoRoles = `[capacity]
masons = 1
[roles.chief_of_staff]
sandbox = "container"
image = "fixture-image"
[roles.mason]
sandbox = "container"
image = "fixture-image"
[roles.reviewer]
sandbox = "container"
image = "fixture-image"
`

const (
	chiefDemoCharter  = "# Charter\n\n1. Keep state in files under the root.\n"
	chiefDemoReply    = "Two workers are building the upload work. I will tell you when they need a decision."
	chiefDemoAnswer   = "In files under the root."
	chiefDemoRuling   = "Keep the upload API as it is and add a new endpoint."
	chiefDemoRelayed  = "The upload API stays unchanged; add a new endpoint for resumable uploads."
	chiefDemoQuestion = "May I change the upload API's response?"
)

// chiefDemoStatus is the status the fake chief of staff writes, with no
// identifier in any field.
var chiefDemoStatus = StatusView{
	Goal:      "Ship resumable uploads.",
	Attention: "",
	Note:      "Two masons and a reviewer are at work on the upload unit.",
	Agents:    []string{"The first mason is building the upload unit.", "The second mason is building the index unit.", "The reviewer is reviewing the upload unit."},
}

// chiefDemoAgents maps each worker thread of the demonstration to its agent.
var chiefDemoAgents = map[string]string{demoThread: demoAgent, "thread_mason2": "agent_mason2", "thread_reviewer": "agent_reviewer"}

func TestM2ChiefOfStaffQuestionsAndInbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// An onboarded project: added through the API, its knowledge base written
	// by a fake librarian and its charter written by the owner.
	lf := newLibrarianFixture(t)
	opts, engine, sessions := lf.opts, lf.engine, lf.sessions
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString(chiefDemoRoles)
	must(t, errors.Join(err, configFile.Close()))
	// Every backend session can be resumed, so a thread's next turn names the
	// session of its previous one.
	engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	seed, err := kb.Seed(lf.clone)
	must(t, err)
	lf.script("extract-1-1", map[string]string{"output/kb/trace.md": "# trace\n\nState lives in files.\n", "output/kb/entities.json": entitiesWith(t, seed, "history")}, nil)

	// The clock stands still, so an event window closes only when the test
	// advances it.
	clock := &fixedClock{now: demoStart}
	opts.Reconciliation.Now = clock.Now
	lives := make(chan *trace.Repository, 1)
	opts = Enforce(opts, Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &demoTransport{sessions: sessions}}})
	production := opts.Threads
	opts.Threads = func(r *trace.Repository, cfg *config.Config) (coreadapter.Reconciler, error) {
		bound, err := production(r, cfg)
		if err != nil {
			return nil, err
		}
		select {
		case <-lives:
		default:
		}
		lives <- r
		// The service grants thread turns to the chief of staff alone. The
		// fake workers are added here with their ask tool; the chief of staff
		// keeps the service's grant and tools.
		turns := bound.(thread.Dispatcher).Runner.Turns.(*questions.Turns).Turns.(*reportingTurns).Turns.(*isolation.Turns)
		turns.Grants = maps.Clone(turns.Grants)
		for _, role := range []string{"mason", "reviewer"} {
			turns.Grants[role] = coreadapter.Capabilities{Tools: []string{questions.AskTool}}
		}
		// The fake workers build no unit: each works in the empty directory
		// the chief of staff is handed, with no workspace to copy back to.
		turns.Workspaces, turns.Capture = stagedWorkspaces{}, nil
		selectChief := turns.Select
		turns.Select = func(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			scope.Role = trace.ChiefOfStaff
			return selectChief(ctx, scope)
		}
		chief := turns.Scoped
		turns.Scoped = func(ctx context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Role == trace.ChiefOfStaff {
				return chief(ctx, scope)
			}
			return questions.Tools(r, chiefDemoAgents[scope.Thread], scope, clock.Now)
		}
		return bound, nil
	}

	s, c := start(t, opts)
	added, err := c.AddProject(ctx, request(lf.clone))
	must(t, err)
	id, traceDir := added.Project.ID, added.Project.Trace
	if x := awaitExtraction(t, c); x.State != "succeeded" {
		t.Fatalf("extraction: %+v", x)
	}
	must(t, os.WriteFile(added.Project.Charter, []byte(chiefDemoCharter), 0600))
	onboarded, err := c.Configuration(ctx)
	must(t, err)
	if cs := onboarded.Project.CharterState; cs == nil || !cs.Ready || cs.Rules != 1 {
		t.Fatalf("charter: %+v", cs)
	}
	c.Close()
	must(t, s.Close())

	// The test fixture creates the workstream: two masons and a reviewer, each
	// with one queued turn.
	cfg, err := config.Load(opts.Config)
	must(t, err)
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, clock.Now(), ownerActor))
	for _, w := range []struct{ agent, thread, role, turn string }{
		{demoAgent, demoThread, "mason", "build"}, {"agent_mason2", "thread_mason2", "mason", "build2"}, {"agent_reviewer", "thread_reviewer", "reviewer", "review"},
	} {
		h := trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: w.agent, Project: id, Workstream: stream, At: clock.Now(), Actor: ownerActor, Cause: "workstream_created"}
		must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: w.role, ThreadID: w.thread}))
		h.Schema, h.ID, h.Cause, h.Depth = "osmia.trace.turn-request", "request_"+w.turn, "message_"+w.turn, 1
		_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: w.agent, ThreadID: w.thread, TurnID: w.turn,
			Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the " + w.role + ".", Prompt: "Work: " + w.turn})
		must(t, err)
	}
	must(t, repo.Close())
	select {
	case <-lives:
	default:
	}

	// From here on a pass runs only when the test sends a tick.
	ticks := make(chan time.Time)
	opts.Reconciliation.Ticks = ticks
	settle := func(s *Service) {
		t.Helper()
		for range 5 {
			select {
			case ticks <- clock.Now():
			case <-time.After(demoTimeout):
				s.Close()
				t.Fatal("service did not finish its pass")
			}
		}
	}
	lifetime := func() (*Service, *Client, *trace.Repository) {
		t.Helper()
		s, err := Start(ctx, opts)
		must(t, err)
		return s, NewClient(s.Socket()), <-lives
	}
	stop := func(s *Service, c *Client) {
		t.Helper()
		c.Close()
		must(t, s.Close())
	}

	// The fake agents. Each records what it saw wrong under mu, and the test
	// fails on it at the end.
	var mu sync.Mutex
	var problems []string
	results := map[string][]string{}
	prompts := map[string]agent.Request{}
	problem := func(args ...any) { problems = append(problems, fmt.Sprintln(args...)) }
	use := func(ctx context.Context, tools *mcp.ClientSession, step, name string, args map[string]any) {
		out, err := callTool(ctx, tools, name, args)
		if err != nil {
			problem(step, err)
		}
		results[step] = append(results[step], out)
	}
	tools := func(ctx context.Context, name string, session *mcp.ClientSession, want ...string) {
		if got, err := toolNames(ctx, session); err != nil || !slices.Equal(got, want) {
			problem("tools of", name, got, err)
		}
	}
	var current *trace.Repository
	worker := func(question string) demoTurn {
		return func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
			mu.Lock()
			defer mu.Unlock()
			prompts[req.Name] = req
			tools(ctx, req.Name, session, questions.AskTool)
			if question != "" {
				use(ctx, session, req.Name, questions.AskTool, map[string]any{"question": question})
				return questionResult(req, "session-"+req.Name, "Asked"), nil
			}
			// The second mason runs while the first waits for its answer.
			th, err := current.Thread(stream, demoAgent)
			if err != nil || !th.Parked() {
				problem("second mason ran before the first parked", err)
			}
			return questionResult(req, "session-"+req.Name, "Built"), nil
		}
	}
	engine.turns["build"] = worker("Where does state live?")
	engine.turns["build2"] = worker("")
	engine.turns["review"] = worker(chiefDemoQuestion)
	resumed := func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		prompts[req.Name] = req
		tools(ctx, req.Name, session, questions.AskTool)
		return questionResult(req, "session-"+req.Name, "Continued"), nil
	}
	var sent []ConversationEntry
	events := map[string]int{}
	engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		prompts[req.Name] = req
		tools(ctx, req.Name, session, "answer", "escalate", "propose_charter", "relay_ruling", "route_amendment", "set_status")
		switch {
		case len(sent) > 0 && req.Name == sent[0].Turn:
			// A status naming an agent by its ID is refused; one in words is
			// stored.
			refused := map[string]any{"goal": chiefDemoStatus.Goal, "note": chiefDemoStatus.Note, "agents": []string{demoAgent + " is building."}}
			use(ctx, session, "status", "set_status", refused)
			use(ctx, session, "status", "set_status", map[string]any{"goal": chiefDemoStatus.Goal, "attention": chiefDemoStatus.Attention, "note": chiefDemoStatus.Note, "agents": chiefDemoStatus.Agents})
			return questionResult(req, "session-chief", chiefDemoReply), nil
		case len(sent) > 1 && req.Name == sent[1].Turn:
			return questionResult(req, "session-chief", "The upload API stays as it is."), nil
		case !strings.HasPrefix(req.Name, "events_"):
		// Prompts replay nothing here: a resumed session receives only the new
		// events, so each case matches one event turn.
		case strings.Contains(req.Prompt, "asked by the mason: Where does state live?"):
			events["questions"]++
			if !strings.Contains(req.Prompt, "asked by the reviewer: "+chiefDemoQuestion) {
				problem("the questions were not delivered together", req.Prompt)
			}
			use(ctx, session, "chief", "answer", map[string]any{"question": "1", "text": chiefDemoAnswer, "citations": []string{"charter#1"}})
			use(ctx, session, "chief", "escalate", map[string]any{"questions": []string{"2"}, "rephrasing": "May the upload API's response change?",
				"blocked": "The reviewer's review of the upload unit.", "options": []string{"Change it", "Keep it and add an endpoint"}, "recommendation": "Keep it and add an endpoint."})
			return questionResult(req, "session-chief", "Answered one, escalated one"), nil
		case strings.Contains(req.Prompt, ", escalation_2 (questions 2): "+chiefDemoRuling):
			events["ruling"]++
			use(ctx, session, "chief", "relay_ruling", map[string]any{"question": "2", "text": chiefDemoRelayed, "scope": "notify"})
			return questionResult(req, "session-chief", "Relayed"), nil
		}
		problem("unexpected chief-of-staff turn", req.Name, req.Prompt)
		return questionResult(req, "session-chief", "Nothing to do"), nil
	}
	engine.turns["answer_1"], engine.turns["answer_2"] = resumed, resumed
	runs := func() map[string]int {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		out := map[string]int{}
		for _, name := range engine.runs {
			switch {
			case strings.HasPrefix(name, "events_"):
				name = "events"
			case strings.HasPrefix(name, "message_"):
				name = "message"
			}
			out[name]++
		}
		return out
	}
	question := func(r *trace.Repository, id string) trace.QuestionState {
		t.Helper()
		list, err := r.Questions(stream)
		must(t, err)
		for _, q := range list {
			if q.Asked.ID == id {
				return q
			}
		}
		t.Fatalf("no question %s in %+v", id, list)
		return trace.QuestionState{}
	}
	threadOf := func(r *trace.Repository, agent string) trace.Thread {
		t.Helper()
		th, err := r.Thread(stream, agent)
		must(t, err)
		return th
	}

	// 1. The owner sends a message; the chief of staff replies and writes the
	// workstream's status.
	s, c, repo = lifetime()
	mu.Lock()
	current = repo
	message, err := c.Send(ctx, stream, "Where do we stand?")
	sent = append(sent, message)
	mu.Unlock()
	must(t, err)
	if message.State != TurnQueued {
		t.Fatalf("sent: %+v", message)
	}
	settle(s)
	list, err := c.Conversation(ctx, stream)
	must(t, err)
	if got := states(list); !slices.Equal(got, []string{"message:done", "response:done"}) || list.Entries[0].Text != "Where do we stand?" || list.Entries[1].Text != chiefDemoReply {
		t.Fatalf("conversation: %+v", list)
	}
	st, err := c.Status(ctx, stream)
	must(t, err)
	if st.Project != id || st.Status == nil {
		t.Fatalf("status: %+v", st)
	}
	written := *st.Status
	written.Revision, written.UpdatedAt = 0, time.Time{}
	if !reflect.DeepEqual(written, chiefDemoStatus) || st.Status.Revision != 1 {
		t.Fatalf("status %+v, want %+v", st.Status, chiefDemoStatus)
	}
	for _, text := range append([]string{written.Goal, written.Attention, written.Note}, written.Agents...) {
		for _, identifier := range []string{string(id), string(stream), "agent_", "thread_", "session-", "message_", "events_", "fixture-image"} {
			if strings.Contains(text, identifier) {
				t.Fatalf("status field %q carries %q", text, identifier)
			}
		}
	}

	// 2. The first mason and the reviewer ask and park. The mason's slot goes
	// to the queued second mason in the meantime.
	for _, agent := range []string{demoAgent, "agent_reviewer"} {
		th := threadOf(repo, agent)
		if !th.Parked() || len(th.Turns) != 1 {
			t.Fatalf("%s: parked %v, turns %+v", agent, th.Parked(), th.Turns)
		}
		if o := th.Turns[0].Response.Result.Outcome; o == nil || o.Status != "waiting" {
			t.Fatalf("%s asking turn outcome: %+v", agent, o)
		}
	}
	if q := question(repo, "1"); q.State != trace.QuestionOpen || q.Asked.Thread != demoThread || q.Asked.Question != "Where does state live?" {
		t.Fatalf("question 1: %+v", q)
	}
	if q := question(repo, "2"); q.State != trace.QuestionOpen || q.Asked.Thread != "thread_reviewer" {
		t.Fatalf("question 2: %+v", q)
	}
	if th := threadOf(repo, "agent_mason2"); th.Parked() || len(th.Turns) != 1 || th.Turns[0].Status() != "idle" {
		t.Fatalf("second mason: %+v", th.Turns)
	}
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"extract-1-1": 1, "message": 1, "build": 1, "build2": 1, "review": 1}) {
		t.Fatalf("runs after asking: %v", got)
	}

	// 3. Once the event window closes, the chief of staff answers the
	// mason's question with a citation and escalates the reviewer's. The
	// mason resumes on its own thread.
	clock.Advance(time.Minute)
	settle(s)
	settle(s)
	mason := threadOf(repo, demoAgent)
	if mason.Parked() || len(mason.Turns) != 2 || mason.Turns[1].Request.TurnID != "answer_1" || mason.Turns[1].Status() != "idle" {
		t.Fatalf("mason after the answer: parked %v, turns %+v", mason.Parked(), mason.Turns)
	}
	if q := question(repo, "1"); q.State != trace.QuestionAnswered || q.Ruling == nil || q.Ruling.Decision != trace.DecisionAnswer || !slices.Equal(q.Ruling.Citations, []string{"charter#1"}) {
		t.Fatalf("answered question: %+v", q)
	}
	if q := question(repo, "2"); q.State != trace.QuestionEscalated || !threadOf(repo, "agent_reviewer").Parked() {
		t.Fatalf("escalated question: %+v", q)
	}

	// 4. The escalation is in the inbox, and still there, once, after a
	// restart.
	inbox, err := c.Inbox(ctx)
	must(t, err)
	want := InboxEntry{Number: 1, Workstream: stream, Batch: "escalation_2", Question: "May the upload API's response change?", Blocked: "The reviewer's review of the upload unit.",
		Options: []string{"Change it", "Keep it and add an endpoint"}, Recommendation: "Keep it and add an endpoint.", EscalatedAt: clock.Now(),
		Asked: []InboxQuestion{{ID: "2", AskedBy: "agent_reviewer", Question: chiefDemoQuestion}}}
	if len(inbox.Entries) != 1 || !reflect.DeepEqual(inbox.Entries[0], want) {
		t.Fatalf("inbox:\n%+v\nwant\n%+v", inbox.Entries, want)
	}
	before := runs()
	stop(s, c)
	s, c, repo = lifetime()
	mu.Lock()
	current = repo
	mu.Unlock()
	settle(s)
	again, err := c.Inbox(ctx)
	must(t, err)
	if !reflect.DeepEqual(again, inbox) {
		t.Fatalf("inbox after the restart:\n%+v\nwant\n%+v", again, inbox)
	}
	if got := runs(); !reflect.DeepEqual(got, before) {
		t.Fatalf("the restart ran %v, had run %v", got, before)
	}

	// 5. The owner rules. The chief of staff rephrases the ruling as a
	// project notice, the reviewer resumes with it, and the next message's
	// context carries the notice.
	answered, err := c.Answer(ctx, want.Number, chiefDemoRuling)
	must(t, err)
	if !reflect.DeepEqual(answered, AnswerResponse{Number: 1, Workstream: stream, Batch: "escalation_2", Questions: []string{"2"}, Ruling: chiefDemoRuling, At: clock.Now()}) {
		t.Fatalf("answer: %+v", answered)
	}
	if inbox, err := c.Inbox(ctx); err != nil || len(inbox.Entries) != 0 {
		t.Fatalf("inbox after the ruling: %+v %v", inbox, err)
	}
	clock.Advance(time.Minute)
	settle(s)
	settle(s)
	reviewer := threadOf(repo, "agent_reviewer")
	if reviewer.Parked() || len(reviewer.Turns) != 2 || reviewer.Turns[1].Request.TurnID != "answer_2" || reviewer.Turns[1].Status() != "idle" {
		t.Fatalf("reviewer after the ruling: parked %v, turns %+v", reviewer.Parked(), reviewer.Turns)
	}
	mu.Lock()
	message, err = c.Send(ctx, stream, "Did the upload API change?")
	sent = append(sent, message)
	mu.Unlock()
	must(t, err)
	settle(s)
	list, err = c.Conversation(ctx, stream)
	must(t, err)
	if got := states(list); len(got) != 4 || got[3] != "response:done" {
		t.Fatalf("conversation after the ruling: %v", got)
	}
	stop(s, c)

	// 6. Only the chief of staff received event turns, and the trace holds
	// the question, the choice, the ruling and what was sent back.
	if got := runs(); !reflect.DeepEqual(got, map[string]int{"extract-1-1": 1, "message": 2, "build": 1, "build2": 1, "review": 1, "events": 2, "answer_1": 1, "answer_2": 1}) {
		t.Fatalf("runs: %v", got)
	}
	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	threads, err := repo.Threads(stream)
	must(t, err)
	eventTurns := 0
	for _, th := range threads {
		for _, q := range th.Turns {
			if strings.HasPrefix(q.Request.TurnID, "events_") {
				if th.Identity.Role != trace.ChiefOfStaff {
					t.Fatalf("%s received event turn %s", th.Identity.ID, q.Request.TurnID)
				}
				eventTurns++
			}
		}
	}
	if eventTurns != 2 {
		t.Fatalf("event turns: %d", eventTurns)
	}
	ruled := question(repo, "2")
	if ruled.State != trace.QuestionAnswered || ruled.Asked.Question != chiefDemoQuestion || ruled.Asked.AskedBy != (trace.Actor{Kind: "agent", ID: "agent_reviewer"}) {
		t.Fatalf("ruled question: %+v", ruled)
	}
	if e := ruled.Latest.Escalation; e == nil || e.Batch != "escalation_2" || e.Recommendation != want.Recommendation || ruled.Latest.SentToOwner != want.Question {
		t.Fatalf("escalation: %+v", ruled.Latest)
	}
	if r := ruled.Ruling; r == nil || r.Decision != trace.DecisionRuling || r.OwnerResponse != chiefDemoRuling || r.ReturnedAnswer != chiefDemoRelayed || r.Scope != "notify" || r.Revision != 2 {
		t.Fatalf("ruling: %+v", r)
	}
	for path, wants := range map[string][]string{
		"workstreams/" + string(stream) + "/questions/2/question.jsonl": {chiefDemoQuestion, `"batch":"escalation_2"`},
		"workstreams/" + string(stream) + "/questions/2/rulings.jsonl":  {`"owner_response":"` + chiefDemoRuling, `"returned_answer":"` + chiefDemoRelayed},
		"workstreams/" + string(stream) + "/questions/1/question.jsonl": {"Where does state live?"},
		"workstreams/" + string(stream) + "/questions/1/rulings.jsonl":  {`"returned_answer":"` + chiefDemoAnswer, `"citations":["charter#1"]`},
	} {
		committed := demoGit(t, "", "-C", traceDir, "cat-file", "blob", "HEAD:"+path)
		for _, w := range wants {
			if !strings.Contains(committed, w) {
				t.Fatalf("committed %s lacks %q:\n%s", path, w, committed)
			}
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// Each asker resumed its own backend session with the answer.
	for turn, w := range map[string]struct{ session, prompt string }{
		"answer_1": {"session-build", "Answer to your question 1.\n\nYou asked:\nWhere does state live?\n\nAnswer:\n" + chiefDemoAnswer + "\n\nCitations:\n- charter#1\n"},
		"answer_2": {"session-review", "The owner ruled on your question 2. The chief of staff relays the ruling.\n\nYou asked:\n" + chiefDemoQuestion + "\n\nAnswer:\n" + chiefDemoRelayed + "\n"},
	} {
		if req := prompts[turn]; req.ResumeID != w.session || !strings.Contains(req.Prompt, w.prompt) {
			t.Fatalf("%s resumed %q with:\n%s", turn, req.ResumeID, req.Prompt)
		}
	}
	notice := "## Notices\n- workstreams/" + string(stream) + "/questions/2/rulings.jsonl (record 2 revision 2, workstream " + string(stream) + ")\n  notice: " + chiefDemoRelayed + "\n"
	if first := prompts[sent[0].Turn].SystemPrompt; !strings.HasSuffix(first, "## Notices\nNo project-wide notices.\n") {
		t.Fatalf("a notice before the ruling:\n%s", first)
	}
	if later := prompts[sent[1].Turn].SystemPrompt; !strings.HasSuffix(later, notice) {
		t.Fatalf("the later bundle lacks the notice:\n%s", later)
	}
	wantResults := map[string][]string{
		"build":  {`{"recorded":true,"question":"1","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
		"review": {`{"recorded":true,"question":"2","next":"End your turn now. The answer arrives as your next turn on this thread."}`},
		"chief": {
			`{"recorded":true,"question":"1","next":"The answer is delivered to the asker as its next turn."}`,
			`{"recorded":true,"batch":"escalation_2","questions":["2"]}`,
			`{"recorded":true,"questions":["2"],"scope":"notify","next":"The ruling is delivered to each asker as its next turn."}`,
		},
	}
	if st := results["status"]; len(st) != 2 || st[0] != `{"stored":false,"reason":"agents[0] contains an Osmia or backend identifier (\"`+demoAgent+`\"); refer to the work or the agent in words"}` || st[1] != `{"stored":true,"revision":1}` {
		t.Fatalf("set_status results: %v", st)
	}
	delete(results, "status")
	if !reflect.DeepEqual(results, wantResults) {
		t.Fatalf("tool results:\n%v\nwant\n%v", results, wantResults)
	}
	if events["questions"] != 1 || events["ruling"] != 1 {
		t.Fatalf("chief-of-staff event turns: %v", events)
	}
	if len(problems) != 0 {
		t.Fatalf("fake agents saw:\n%s", strings.Join(problems, "\n"))
	}
}
