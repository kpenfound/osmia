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

	"github.com/kpenfound/agentcore/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// crossRepoWiringFixture is two active projects in one running service, each
// with its own trace and workstream, wired so a real chief-of-staff turn
// runs the production question tools over MCP against whichever repository
// Options.Threads is called with: either project's own, for an ordinary
// workstream's worker, or a cross-repo workgroup's own trace, for the
// shared chief of staff that production's own workgroupChiefDispatchPass
// resolves to.
type crossRepoWiringFixture struct {
	opts               Options
	cfg                *config.Config
	clock              *fixedClock
	engine             *demoEngine
	ticks              chan time.Time
	project1, project2 config.ProjectID
	stream1, stream2   config.WorkstreamID
	mu                 sync.Mutex
	live               map[config.ProjectID]*trace.Repository
	problems           []string
}

// newCrossRepoWiringFixture builds the two projects, each with one
// workstream and one mason thread ready to ask a question. agents maps
// every thread ID this test will claim a turn on, across both projects and
// the shared chief-of-staff thread, to the agent ID that thread's turns are
// recorded under.
func newCrossRepoWiringFixture(t *testing.T, agents map[string]string) *crossRepoWiringFixture {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "wgw-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	root := opts.Config.Root
	top, err := os.ReadFile(filepath.Join(root, "config.toml"))
	must(t, err)
	listed := strings.Replace(string(top), fmt.Sprintf("active_projects = [%q]", project), fmt.Sprintf("active_projects = [%q, %q]", project, otherProject), 1)
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(listed), 0600))
	home2 := filepath.Join(home, "repo2")
	must(t, os.MkdirAll(filepath.Join(root, "projects", string(otherProject)), 0700))
	must(t, os.WriteFile(filepath.Join(root, "projects", string(otherProject), "config.toml"), []byte(`version = 1
upstream = "upstream/other"
fork = "owner/other"
clone = "`+home2+`"
`), 0600))
	opts.Workstreams = nil
	cfg, err := config.Load(opts.Config)
	must(t, err)

	clock := &fixedClock{now: demoStart}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	f := &crossRepoWiringFixture{opts: opts, cfg: cfg, clock: clock, project1: project, project2: otherProject, stream1: stream, stream2: otherStream,
		ticks: make(chan time.Time), live: map[config.ProjectID]*trace.Repository{}}
	for _, p := range cfg.Projects {
		must(t, os.MkdirAll(p.Clone, 0700))
		repo, err := trace.Create(ctx, cfg.Root, p, clock.Now(), owner)
		must(t, err)
		ws, agentID, threadID := f.stream1, demoAgent, demoThread
		if p.ID == f.project2 {
			ws, agentID, threadID = f.stream2, "agent_mason2", "thread_mason2"
		}
		must(t, repo.CreateWorkstream(ctx, ws, clock.Now(), owner))
		must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: agentID, Project: p.ID, Workstream: ws, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: "mason", ThreadID: threadID}))
		// The chief's answer to member1's question cites charter#1, which
		// must resolve against member1's own project (through ProjectFor),
		// exactly as newQuestionFixture's own raw charter.md write lets its
		// citation resolve.
		traceDir, err := cfg.Root.ProjectTrace(p.ID)
		must(t, err)
		must(t, os.WriteFile(filepath.Join(traceDir, "charter.md"), []byte("# Charter\n\n1. Keep state in files under the root.\n"), 0600))
		must(t, repo.Close())
	}

	sessions := &demoSessions{byKey: map[string]*mcp.ClientSession{}}
	f.engine = &demoEngine{sessions: sessions, turns: map[string]demoTurn{}}
	f.engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	views := filepath.Join(cfg.Root.String(), "views")
	must(t, os.Mkdir(views, 0700))
	every := append([]string{"file_read", questions.AskTool}, questions.ChiefTools...)
	f.opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		f.mu.Lock()
		f.live[r.Project()] = r
		f.mu.Unlock()
		turns := &isolation.Turns{Workspaces: &demoWorkspaces{}, Views: isolation.Views{Directory: views}, Engine: f.engine,
			Grants: map[string]coreadapter.Capabilities{"mason": {Tools: every}, "reviewer": {Tools: every}, trace.ChiefOfStaff: {Tools: every}},
			Select: func(context.Context, coreadapter.Scope) (isolation.Selection, error) {
				return isolation.Selection{Execution: coreadapter.ExecutionSettings{Mode: "container", Image: "fixture-image"}}, nil
			},
			Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
				return questions.Tools(r, agents[scope.Thread], scope, clock.Now)
			},
			Hosts: &coreadapter.MCPHost{Transport: &demoTransport{sessions: sessions}},
		}
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: &questions.Turns{Turns: turns, Repository: r}, Now: clock.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				directory := filepath.Join(cfg.Root.String(), "sessions", string(r.Project()), in.Agent, in.Turn)
				return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
			}}, nil
	}
	f.opts.Reconciliation.Now, f.opts.Reconciliation.Ticks = clock.Now, f.ticks
	return f
}

// settle sends several ticks, so every pass they caused has finished by the
// time it returns.
func (f *crossRepoWiringFixture) settle(t *testing.T, s *Service) {
	t.Helper()
	for range 8 {
		select {
		case f.ticks <- f.clock.Now():
		case <-time.After(demoTimeout):
			s.Close()
			t.Fatal("service did not finish its pass")
		}
	}
}

func (f *crossRepoWiringFixture) problem(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	f.problems = append(f.problems, msg)
}

func TestWorkgroupChiefDispatchVisitsLeaderAfterRetainedSourceLocatorAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCrossRepoWiringFixture(t, map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff})
	f.engine.turns["*"] = func(_ context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		return questionResult(req, "session-chief", "The agreed work continues."), nil
	}
	s, err := Start(ctx, f.opts)
	must(t, err)
	defer func() { must(t, s.Close()) }()
	id, api := s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatal(api)
	}
	leader := leastWorkstream([]config.WorkstreamID{f.stream1, f.stream2})
	project := f.project1
	if leader == f.stream2 {
		project = f.project2
	}
	repo, err := s.repository(project)
	must(t, err)
	const source config.WorkstreamID = "w_00000000000000000000000000000000"
	if source >= leader {
		t.Fatal("retained source must sort before the dispatch leader")
	}
	must(t, repo.CreateWorkstream(ctx, source, f.clock.Now(), ownerActor))
	must(t, repo.RetractChiefOfStaff(ctx, source))
	must(t, repo.RecordCrossRepoWorkgroup(ctx, source, id, f.clock.Now(), ownerActor, "design-start"))
	for lifetime := 0; lifetime < 2; lifetime++ {
		if lifetime > 0 {
			must(t, s.Close())
			s, err = Start(ctx, f.opts)
			must(t, err)
		}
		c := NewClient(s.Socket())
		sent, err := c.Send(ctx, leader, "Report the workgroup's status.")
		must(t, err)
		wait, closeWait := serviceChanges(t, s)
		deadline := time.Now().Add(demoTimeout)
		for {
			f.settle(t, s)
			conversation, err := c.Conversation(ctx, leader)
			must(t, err)
			if slices.ContainsFunc(conversation.Entries, func(e ConversationEntry) bool {
				return e.Turn == sent.Turn && e.Kind == "response" && e.State == TurnDone && e.Text == "The agreed work continues."
			}) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("lifetime %d: source %s consumed leader %s's dispatch: %+v", lifetime, source, leader, conversation)
			}
			wait()
		}
		closeWait()
		c.Close()
	}
}

// repositories waits for both projects' own, now-live repository instances:
// Options.Threads records every repository it is called with, including a
// cross-repo workgroup's own trace on the calls workgroupChiefDispatchPass
// makes, so this polls for the two real projects' own entries by ID rather
// than reading a fixed number of calls off a channel.
func (f *crossRepoWiringFixture) repositories(t *testing.T, s *Service) (repo1, repo2 *trace.Repository) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		f.mu.Lock()
		repo1, repo2 = f.live[f.project1], f.live[f.project2]
		f.mu.Unlock()
		if repo1 != nil && repo2 != nil {
			return repo1, repo2
		}
		if time.Now().After(deadline) {
			s.Close()
			t.Fatal("both projects' own repositories were never opened")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCrossRepoWorkgroupWiringAcrossARestart is the service-level two-repo
// test spec#4's and spec#5's dispatch acceptance rests on: a role in each
// member asks, both questions reach the one shared chief thread (through
// production event delivery, never a thread of either member's own
// repository), the chief's answer tool runs against the workgroup's own
// trace and resumes each asker in its own repo. A real service restart in
// the middle drops and duplicates nothing. The chief's escalate,
// relay_ruling and the owner's inbox for a cross-repo workgroup belong to
// unit wg-inbox, built on this dispatch; they are not exercised here.
func TestCrossRepoWorkgroupWiringAcrossARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const question1, question2 = "Where does the server store chunks?", "Does the client ever see the offset?"
	const answer1, answer2 = "In a content-addressed store.", "No, only the server does."

	agents := map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff, "thread_mason": demoAgent, "thread_mason2": "agent_mason2"}
	f := newCrossRepoWiringFixture(t, agents)
	opts, clock := f.opts, f.clock

	ask := func(question string) demoTurn {
		return func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
			out, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": question})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				f.problem("%s ask: %s %v", req.Name, out, err)
			}
			return questionResult(req, "session-"+req.Name, "Asked"), nil
		}
	}
	f.engine.turns["build1"] = ask(question1)
	f.engine.turns["build2"] = ask(question2)

	var flagMu sync.Mutex
	var answered1, answered2 bool
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		flagMu.Lock()
		needAnswer2 := !answered2 && strings.Contains(req.Prompt, "asked by the mason: "+question2)
		needAnswer1 := !answered1 && strings.Contains(req.Prompt, "asked by the mason: "+question1)
		flagMu.Unlock()
		acted := false
		// Neither member is the workgroup's own synthetic host, so each
		// member's own question must be named qualified.
		if needAnswer2 {
			out, err := callTool(ctx, tools, questions.AnswerTool, map[string]any{"question": string(f.stream2) + ":1", "text": answer2, "citations": []string{"charter#1"}})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				f.problem("answer2: %s %v", out, err)
			}
			flagMu.Lock()
			answered2 = true
			flagMu.Unlock()
			acted = true
		}
		if needAnswer1 {
			out, err := callTool(ctx, tools, questions.AnswerTool, map[string]any{"question": string(f.stream1) + ":1", "text": answer1, "citations": []string{"charter#1"}})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				f.problem("answer1: %s %v", out, err)
			}
			flagMu.Lock()
			answered1 = true
			flagMu.Unlock()
			acted = true
		}
		if acted {
			return questionResult(req, "session-chief", "Handled"), nil
		}
		if strings.Contains(req.Prompt, "asked by the mason: "+question1) || strings.Contains(req.Prompt, "asked by the mason: "+question2) {
			return questionResult(req, "session-chief", "Nothing further"), nil
		}
		f.problem("unexpected chief-of-staff turn:\n%s", req.Prompt)
		return questionResult(req, "session-chief", "Nothing to do"), nil
	}
	f.engine.turns[questions.TurnID("1")] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		return questionResult(req, "session-"+req.Name, "Continued"), nil
	}

	s, err := Start(ctx, opts)
	must(t, err)
	closed := false
	defer func() {
		if !closed {
			s.Close()
		}
	}()
	repo1, repo2 := f.repositories(t, s)

	id, api := s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatalf("draft workgroup: %v", api)
	}

	// Each member's own worker asks its own question: a notice is recorded
	// on its own trace, which production event delivery must carry to the
	// shared chief-of-staff thread, since this workgroup spans two repos.
	_, err = repo1.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_build1", Revision: 1, Project: f.project1, Workstream: f.stream1, At: clock.Now(), Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "test"},
		AgentID: demoAgent, ThreadID: "thread_mason", TurnID: "build1", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the mason.", Prompt: "Work: build1"})
	must(t, err)
	_, err = repo2.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_build2", Revision: 1, Project: f.project2, Workstream: f.stream2, At: clock.Now(), Actor: trace.Actor{Kind: "owner", ID: "local"}, Cause: "test"},
		AgentID: "agent_mason2", ThreadID: "thread_mason2", TurnID: "build2", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the mason.", Prompt: "Work: build2"})
	must(t, err)

	f.settle(t, s)
	clock.Advance(time.Minute)

	bothAnswered := func() bool {
		q1, err := repo1.Questions(f.stream1)
		if err != nil || len(q1) != 1 || q1[0].State != trace.QuestionAnswered {
			return false
		}
		q2, err := repo2.Questions(f.stream2)
		if err != nil || len(q2) != 1 || q2[0].State != trace.QuestionAnswered {
			return false
		}
		return true
	}
	deadline := time.Now().Add(demoTimeout)
	for !bothAnswered() {
		select {
		case f.ticks <- clock.Now():
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			f.mu.Lock()
			probs := slices.Clone(f.problems)
			f.mu.Unlock()
			s.Close()
			t.Fatalf("both questions were not answered in time; problems: %v", probs)
		}
	}

	// Neither member's own repository ever got a chief-of-staff thread of
	// its own: the shared thread lives only in the workgroup's own trace.
	if _, err := repo1.ChiefOfStaffThread(f.stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member1's own repository got a chief-of-staff thread: %v", err)
	}
	if _, err := repo2.ChiefOfStaffThread(f.stream2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member2's own repository got a chief-of-staff thread: %v", err)
	}

	// Each answer resumes its own asker, in its own repo, with the answer's
	// own text: nothing leaked across members.
	resumedWith := func(repo *trace.Repository, stream config.WorkstreamID, agentID, text string) bool {
		th, err := repo.Thread(stream, agentID)
		if err != nil {
			return false
		}
		i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == questions.TurnID("1") })
		return i >= 0 && strings.Contains(th.Turns[i].Request.Prompt, text)
	}
	deadline = time.Now().Add(demoTimeout)
	for !(resumedWith(repo1, f.stream1, demoAgent, answer1) && resumedWith(repo2, f.stream2, "agent_mason2", answer2)) {
		select {
		case f.ticks <- clock.Now():
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			f.mu.Lock()
			probs := slices.Clone(f.problems)
			f.mu.Unlock()
			s.Close()
			t.Fatalf("one or both askers were not resumed with their own answer in time; problems: %v", probs)
		}
	}

	// Restart: both answers are already delivered, and nothing about the
	// recorded trace state is lost or duplicated.
	must(t, s.Close())
	closed = true
	s2, err := Start(ctx, opts)
	must(t, err)
	closed2 := false
	defer func() {
		if !closed2 {
			s2.Close()
		}
	}()
	repo1b, repo2b := f.repositories(t, s2)

	th1Before, err := repo1b.Thread(f.stream1, demoAgent)
	must(t, err)
	th2Before, err := repo2b.Thread(f.stream2, "agent_mason2")
	must(t, err)
	before1, before2 := slices.Clone(th1Before.Turns), slices.Clone(th2Before.Turns)

	clock.Advance(time.Minute)
	f.settle(t, s2)

	th1After, err := repo1b.Thread(f.stream1, demoAgent)
	must(t, err)
	th2After, err := repo2b.Thread(f.stream2, "agent_mason2")
	must(t, err)
	if len(th1After.Turns) != len(before1) {
		t.Fatalf("member1's own thread duplicated a turn across the restart: before=%+v after=%+v", before1, th1After.Turns)
	}
	if len(th2After.Turns) != len(before2) {
		t.Fatalf("member2's own thread duplicated a turn across the restart: before=%+v after=%+v", before2, th2After.Turns)
	}
	q1After, err := repo1b.Questions(f.stream1)
	must(t, err)
	if q1After[0].State != trace.QuestionAnswered || q1After[0].Ruling == nil || q1After[0].Ruling.ReturnedAnswer != answer1 {
		t.Fatalf("member1's question regressed across the restart: %+v", q1After[0])
	}
	q2After, err := repo2b.Questions(f.stream2)
	must(t, err)
	if q2After[0].State != trace.QuestionAnswered || q2After[0].Ruling == nil || q2After[0].Ruling.ReturnedAnswer != answer2 {
		t.Fatalf("member2's question regressed across the restart: %+v", q2After[0])
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.problems) != 0 {
		t.Fatal(strings.Join(f.problems, "\n"))
	}
	_ = id
}

// driveTicksUntil sends ticks to f's reconciliation loop, retrying on a
// short timeout, until condition holds or demoTimeout passes: the pass that
// this test blocks never finishes, so a tick aimed at it is never received,
// and only a send that keeps retrying lets the other project's own loop,
// and this one before it blocks, still make progress.
func (f *crossRepoWiringFixture) driveTicksUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for !condition() {
		select {
		case f.ticks <- f.clock.Now():
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("condition was never reached")
		}
	}
}

// TestOwnerMessageAndConversationReadStayResponsiveWhileAWorkgroupChiefTurnRuns
// covers the second acceptance item: while workgroupChiefDispatchPass's own
// call to dispatchWorkgroupChiefTurn is inside a model turn on the shared
// chief-of-staff thread, holding the workgroup's own trace open the whole
// time, a second owner message to the other member and a conversation read
// of the first still answer within the service's own read-latency bound.
// Were the workgroup trace's exclusive lock, the OS flock or the
// in-process mutex workgroupProcessLock serializes it behind, held across
// that call, either request would wait for the blocked turn to finish
// instead (acquireWorkgroupTrace, workgroup_trace.go).
func TestOwnerMessageAndConversationReadStayResponsiveWhileAWorkgroupChiefTurnRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	agents := map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff, "thread_mason": demoAgent, "thread_mason2": "agent_mason2"}
	f := newCrossRepoWiringFixture(t, agents)
	opts := f.opts

	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return questionResult(req, "session-chief", "Handled"), nil
	}

	s, err := Start(ctx, opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	c := NewClient(s.Socket())
	t.Cleanup(func() { c.Close() })
	f.repositories(t, s)

	if _, api := s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2}); api != nil {
		t.Fatalf("draft workgroup: %v", api)
	}

	// The owner's first message to member1 creates the shared chief thread
	// and enqueues its first turn, which the fake engine above blocks.
	if _, err := c.Send(ctx, f.stream1, "First message"); err != nil {
		t.Fatalf("send first message: %v", err)
	}

	startedFired := func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	}
	f.driveTicksUntil(t, startedFired)

	// While that turn is blocked inside reconciler.Apply, holding the
	// workgroup's own trace open, a second owner message to member2 and a
	// conversation read of member1 must each still answer within the
	// service's own read-latency bound.
	begin := time.Now()
	if _, err := c.Send(ctx, f.stream2, "Second message, to the other member"); err != nil {
		t.Fatalf("send second message: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > coreReadBound {
		t.Errorf("owner message to the other member took %s, want within %s", elapsed, coreReadBound)
	}

	begin = time.Now()
	conv, err := c.Conversation(ctx, f.stream1)
	if err != nil {
		t.Fatalf("conversation read: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > coreReadBound {
		t.Errorf("conversation read took %s, want within %s", elapsed, coreReadBound)
	}
	if len(conv.Entries) == 0 || conv.Entries[0].Text != "First message" {
		t.Fatalf("conversation does not show the first message: %+v", conv.Entries)
	}
}

// TestCrossRepoWorkgroupFromRealSealExcludesStartupWrappingAcrossARestart
// verifies completed production sealing and restart preserve one workgroup
// and shared chief, with no member wrapping or duplicate conversations.
func TestCrossRepoWorkgroupFromRealSealExcludesStartupWrappingAcrossARestart(t *testing.T) {
	t.Parallel()
	f := newCrossRepoRatifyFixture(t)
	defer f.stop(t)
	ctx := context.Background()

	// Both registered upstreams are real local bare repos for the production
	// fetch, branch and seal operations.
	main1 := f.upstream(t)
	home := filepath.Dir(f.clone)
	clone2 := filepath.Join(home, "clone2")
	bare2 := filepath.Join(home, "remotes", "owner", "other.git")
	must(t, os.MkdirAll(filepath.Dir(bare2), 0700))
	demoGit(t, home, "-C", clone2, "branch", "-M", "main")
	demoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone2, bare2)
	demoGit(t, home, "-C", clone2, "remote", "add", "upstream", bare2)
	main2 := strings.TrimSpace(demoGit(t, home, "-C", bare2, "rev-parse", "refs/heads/main"))

	id, api := f.s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatalf("draft workgroup: %v", api)
	}
	packets := f.awaitDecisionPackets(t)
	if _, err := f.c.Ratify(ctx, f.stream1, packets[0].Packet.Revision.Spec, packets[0].Packet.Revision.Plan); err != nil {
		t.Fatalf("ratify: %v", err)
	}

	checkCompletedSeals := func(t *testing.T) trace.GroupApproval {
		t.Helper()
		wait, closeWait := serviceChanges(t, f.s)
		defer closeWait()
		deadline := time.Now().Add(demoTimeout)
		var decision trace.GroupApproval
		for {
			repo, err := f.s.repository(f.project)
			must(t, err)
			var found bool
			decision, found, err = repo.GroupApprovalOf(f.stream1)
			must(t, err)
			if found && decision.Complete {
				break
			}
			if time.Now().After(deadline) {
				var diagnostic strings.Builder
				for _, packet := range packets {
					r, err := f.s.repository(packet.Project)
					must(t, err)
					state, err := r.Workflow(packet.Workstream, trace.FeatureSubject)
					must(t, err)
					ops, err := r.Operations(packet.Workstream)
					must(t, err)
					fmt.Fprintf(&diagnostic, "member=%s state=%s operations=%+v\n", packet.Workstream, state.Value, ops)
				}
				t.Fatalf("group sealing did not complete: found=%v decision=%+v\n%s", found, decision, diagnostic.String())
			}
			wait()
		}
		if decision.Workgroup != id || len(decision.Members) != 2 {
			t.Fatalf("completed approval has wrong membership: %+v", decision)
		}
		for i, packet := range packets {
			repo, err := f.s.repository(packet.Project)
			must(t, err)
			approval, found, err := repo.GroupApprovalOf(packet.Workstream)
			must(t, err)
			if !found || !equalJSONTest(approval, decision) || !slices.ContainsFunc(decision.Members, func(pin trace.GroupMemberApproval) bool {
				return pin.Project == packet.Project && pin.Workstream == packet.Workstream && pin.Spec == packet.Packet.Revision.Spec && pin.Plan == packet.Packet.Revision.Plan
			}) {
				t.Fatalf("member %s lost its exact approval pins: %+v", packet.Workstream, approval)
			}
			sealed, doc, found, err := seal.Latest(repo, packet.Workstream)
			must(t, err)
			if !found || doc.Revision != 1 || sealed.Revision != packet.Packet.Revision || sealed.Base.Remote != "upstream" || sealed.Base.Branch != "main" || sealed.Base.Commit != [2]string{main1, main2}[i] || sealed.Branch != featureBranch(packet.Workstream) {
				t.Fatalf("member %s has no completed pinned seal: %+v document=%+v found=%v", packet.Workstream, sealed, doc, found)
			}
		}
		return decision
	}
	completed := checkCompletedSeals(t)

	// Right after the seal: the workgroup record exists in the
	// workgroup-level trace, naming the host and both real members, read
	// from either member's own pointer.
	checkWorkgroupRecord := func(t *testing.T) {
		t.Helper()
		repo1, err := f.s.repository(f.project)
		must(t, err)
		repo2, err := f.s.repository(f.project2)
		must(t, err)
		gid1, err := repo1.CrossRepoWorkgroupOf(f.stream1)
		must(t, err)
		gid2, err := repo2.CrossRepoWorkgroupOf(f.stream2)
		must(t, err)
		if gid1 != id || gid2 != id {
			t.Fatalf("members do not both point to the sealed workgroup: member1=%s member2=%s want=%s", gid1, gid2, id)
		}
		_, g, found, err := repo1.CrossRepoProjectSpec(id)
		must(t, err)
		if !found || len(g.Members) != 3 || g.Members[1] != f.stream1 || g.Members[2] != f.stream2 {
			t.Fatalf("workgroup record is not exactly the host and the two real members: %+v found=%v", g, found)
		}
	}
	checkWorkgroupRecord(t)

	// The shared chief-of-staff thread lives only in the workgroup-level
	// trace: the owner's first message to each member, through the real
	// production path, gives neither member's own repository a thread of
	// its own.
	repo1, err := f.s.repository(f.project)
	must(t, err)
	repo2, err := f.s.repository(f.project2)
	must(t, err)
	sent1, err := f.c.Send(ctx, f.stream1, "Where does the acknowledgment live?")
	must(t, err)
	if _, err := repo1.ChiefOfStaffThread(f.stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member1's own repository got a chief-of-staff thread of its own: %v", err)
	}
	sent2, err := f.c.Send(ctx, f.stream2, "Does the client ever see the offset?")
	must(t, err)
	if _, err := repo2.ChiefOfStaffThread(f.stream2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member2's own repository got a chief-of-staff thread of its own: %v", err)
	}

	// Each member's own conversation and feed resolve to the one shared
	// thread: both see both messages, in order.
	checkShared := func(t *testing.T) {
		t.Helper()
		for _, m := range []config.WorkstreamID{f.stream1, f.stream2} {
			conv, err := f.c.Conversation(ctx, m)
			must(t, err)
			if len(conv.Entries) != 2 || conv.Entries[0].Turn != sent1.Turn || conv.Entries[1].Turn != sent2.Turn {
				t.Fatalf("workstream %s's shared conversation: %+v", m, conv.Entries)
			}
			var feed FeedResponse
			must(t, f.c.Do(ctx, "GET", Prefix+"/feed/"+string(m), nil, &feed))
			var sawFirst, sawSecond bool
			for _, e := range feed.Entries {
				sawFirst = sawFirst || e.Turn == sent1.Turn
				sawSecond = sawSecond || e.Turn == sent2.Turn
			}
			if !sawFirst || !sawSecond {
				t.Fatalf("workstream %s's feed does not resolve to the shared thread: %+v", m, feed.Entries)
			}
		}
	}
	checkShared(t)

	// Restart: ensureWorkgroups and ensureChiefsOfStaff both run again on
	// start, and must still exclude a cross-repo member.
	f.stop(t)
	f.start(t)
	if after := checkCompletedSeals(t); !equalJSONTest(after, completed) {
		t.Fatalf("restart changed the completed approval: before=%+v after=%+v", completed, after)
	}

	repo1, err = f.s.repository(f.project)
	must(t, err)
	repo2, err = f.s.repository(f.project2)
	must(t, err)

	// Neither member gained its own one-workstream workgroup.
	groups1, _, err := trace.Workgroups(repo1)
	must(t, err)
	if len(groups1) != 0 {
		t.Fatalf("member1's own repository was wrapped in its own one-workstream workgroup: %+v", groups1)
	}
	groups2, _, err := trace.Workgroups(repo2)
	must(t, err)
	if len(groups2) != 0 {
		t.Fatalf("member2's own repository was wrapped in its own one-workstream workgroup: %+v", groups2)
	}

	// Neither member gained a chief-of-staff thread of its own.
	if _, err := repo1.ChiefOfStaffThread(f.stream1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member1's own repository got a chief-of-staff thread after restart: %v", err)
	}
	if _, err := repo2.ChiefOfStaffThread(f.stream2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member2's own repository got a chief-of-staff thread after restart: %v", err)
	}

	// Nothing is duplicated: the workgroup record is still exactly the
	// host and the two real members, and the owner's conversation and
	// feed for each member still show the same two messages, not four.
	checkWorkgroupRecord(t)
	checkShared(t)
}
