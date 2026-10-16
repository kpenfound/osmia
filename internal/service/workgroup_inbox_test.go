package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// unrelatedStream1 and unrelatedStream2 are each their own one-workstream
// workgroup, in project and otherProject respectively, never joining the
// cross-repo workgroup crossRepoInboxFixture also builds: their escalations
// must stay separate from the consolidated one and from each other.
const (
	unrelatedStream1 config.WorkstreamID = "w_11111111111111111111111111111111"
	unrelatedStream2 config.WorkstreamID = "w_22222222222222222222222222222222"
)

// crossRepoInboxFixture is two active projects, each holding one member of a
// shared cross-repo workgroup plus one unrelated workstream of its own, with
// a real chief-of-staff turn running the production question tools over MCP
// against whatever repository Options.Threads is called with: either
// project's own, for an unrelated workstream's ordinary chief, or the
// cross-repo workgroup's own trace, for the shared chief
// workgroupChiefDispatchPass resolves to.
type crossRepoInboxFixture struct {
	cfg                Options
	clock              *fixedClock
	engine             *demoEngine
	ticks              chan time.Time
	project1, project2 config.ProjectID
	stream1, stream2   config.WorkstreamID
	mu                 sync.Mutex
	live               map[config.ProjectID]*trace.Repository
	problems           []string
	lifecycle          []string
	passes             map[config.ProjectID]int
}

// newCrossRepoInboxFixture builds the two projects. Each has one member of
// the eventual cross-repo workgroup and one unrelated workstream, every one
// with its own mason thread. Asking turns are queued after the live join.
func newCrossRepoInboxFixture(t *testing.T) *crossRepoInboxFixture {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "wgi-")
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
	f := &crossRepoInboxFixture{cfg: opts, clock: clock, project1: project, project2: otherProject, stream1: stream, stream2: otherStream,
		ticks: make(chan time.Time), live: map[config.ProjectID]*trace.Repository{}}
	f.cfg.Logger = slog.New(crossRepoInboxLog{fixture: f, Handler: slog.NewJSONHandler(os.Stderr, nil)})
	type member struct {
		stream        config.WorkstreamID
		agent, thread string
	}
	byProject := map[config.ProjectID][]member{
		project:      {{f.stream1, demoAgent, demoThread}, {unrelatedStream1, "agent_u1", "thread_u1"}},
		otherProject: {{f.stream2, "agent_mason2", "thread_mason2"}, {unrelatedStream2, "agent_u2", "thread_u2"}},
	}
	for _, p := range cfg.Projects {
		must(t, os.MkdirAll(p.Clone, 0700))
		repo, err := trace.Create(ctx, cfg.Root, p, clock.Now(), owner)
		must(t, err)
		for _, m := range byProject[p.ID] {
			must(t, repo.CreateWorkstream(ctx, m.stream, clock.Now(), owner))
			must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: m.agent, Project: p.ID, Workstream: m.stream, At: clock.Now(), Actor: owner, Cause: "workstream_created"}, Role: "mason", ThreadID: m.thread}))
		}
		// The chief's answer to either cross-repo member's question cites
		// charter#1, which must resolve against that member's own project
		// (through ProjectFor), exactly as the workgroup-wiring fixture's
		// own raw charter.md write lets its citation resolve.
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
	agents := map[string]string{trace.ChiefOfStaff: trace.ChiefOfStaff, "thread_mason": demoAgent, "thread_mason2": "agent_mason2", "thread_u1": "agent_u1", "thread_u2": "agent_u2"}
	every := append([]string{"file_read", questions.AskTool}, questions.ChiefTools...)
	f.cfg.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
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
	f.cfg.Reconciliation.Now, f.cfg.Reconciliation.Ticks = clock.Now, f.ticks
	return f
}

// crossRepoInboxLog retains controller failures and stop reasons without
// mixing another parallel test's logs into the boundary diagnostics.
type crossRepoInboxLog struct {
	slog.Handler
	fixture *crossRepoInboxFixture
}

func (h crossRepoInboxLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.Handler = h.Handler.WithAttrs(attrs)
	return h
}
func (h crossRepoInboxLog) WithGroup(name string) slog.Handler {
	h.Handler = h.Handler.WithGroup(name)
	return h
}
func (h crossRepoInboxLog) Handle(ctx context.Context, r slog.Record) error {
	err := h.Handler.Handle(ctx, r)
	if r.Level < slog.LevelError && r.Message != "project stopped" && r.Message != "service stopped" {
		return err
	}
	parts := []string{r.Message}
	r.Attrs(func(a slog.Attr) bool {
		parts = append(parts, a.Key+"="+a.Value.String())
		return true
	})
	h.fixture.mu.Lock()
	h.fixture.lifecycle = append(h.fixture.lifecycle, strings.Join(parts, " "))
	h.fixture.mu.Unlock()
	return err
}

// observePasses adds a read-only completion marker after each project's
// production schedule hooks. The immutable stage copy is published through
// the pipeline's existing atomic pointer; the live controller calls it.
func (f *crossRepoInboxFixture) observePasses(s *Service) {
	f.mu.Lock()
	f.passes = map[config.ProjectID]int{}
	f.mu.Unlock()
	s.mu.Lock()
	projects := slices.Clone(s.projects)
	s.mu.Unlock()
	for _, p := range projects {
		observed := *p.pipeline.current.Load()
		observed.hooks = append(slices.Clone(observed.hooks), scheduleHook{"inbox-fixture-observer", func(context.Context) error {
			f.mu.Lock()
			f.passes[p.id]++
			f.mu.Unlock()
			return nil
		}})
		p.pipeline.current.Store(&observed)
	}
}

func (f *crossRepoInboxFixture) diagnostics(s *Service) string {
	f.mu.Lock()
	problems, lifecycle := slices.Clone(f.problems), slices.Clone(f.lifecycle)
	passes := fmt.Sprint(f.passes)
	f.mu.Unlock()
	state := "live"
	select {
	case <-s.done:
		state = fmt.Sprintf("stopped: %v", s.Wait())
	default:
		if !s.ready.Load() || s.lifetime.Err() != nil {
			state = fmt.Sprintf("stopping: %v", s.lifetime.Err())
		}
	}
	// Trace diagnostics are bounded too: a stuck trace lock must not hide
	// the service's stop reason or active backend operations.
	durable := make(chan string, 1)
	go func() {
		_, details := f.snapshot(s)
		durable <- details
	}()
	var details string
	select {
	case details = <-durable:
	case <-time.After(200 * time.Millisecond):
		details = "trace snapshot blocked"
	}
	return fmt.Sprintf("service=%s ready=%v completed schedules=%s lifecycle=%v activity=%+v durable=%s tool problems=%v", state, s.ready.Load(), passes, lifecycle, s.activityView().Activity, details, problems)
}

// snapshot reads durable completion of every worker and chief turn and
// acknowledgements of events whose delivery window has closed.
func (f *crossRepoInboxFixture) snapshot(s *Service) (bool, string) {
	s.mu.Lock()
	projects := slices.Clone(s.projects)
	s.mu.Unlock()
	if len(projects) != 2 {
		return false, fmt.Sprintf("active projects=%d", len(projects))
	}
	f.mu.Lock()
	complete := f.passes[f.project1] > 0 && f.passes[f.project2] > 0
	f.mu.Unlock()
	var details []string
	inspect := func(repo *trace.Repository, stream config.WorkstreamID) {
		threads, err := repo.Threads(stream)
		if err != nil {
			complete = false
			details = append(details, fmt.Sprintf("%s/%s threads: %v", repo.Project(), stream, err))
			return
		}
		for _, th := range threads {
			details = append(details, fmt.Sprintf("thread=%s/%s/%s status=%s active=%q", repo.Project(), stream, th.Identity.ID, th.Status, th.Active))
			complete = complete && th.Active == ""
			for _, q := range th.Turns {
				response, failure := "none", ""
				if q.Response != nil {
					response, failure = q.Response.ID, q.Response.Failure
				}
				details = append(details, fmt.Sprintf("%s/%s/%s/%s request=%s response=%s status=%s completed=%s attempts=%d failure=%q", repo.Project(), stream, th.Identity.ID, q.Request.TurnID, q.Request.ID, response, q.Status(), q.CompletedAt.Format(time.RFC3339Nano), len(q.Attempts), failure))
				complete = complete && !q.CompletedAt.IsZero() && q.Response != nil && (q.Status() == "idle" || q.Status() == "waiting")
			}
		}
		outbox, err := repo.Outbox(stream)
		if err != nil {
			complete = false
			details = append(details, fmt.Sprintf("%s/%s outbox: %v", repo.Project(), stream, err))
			return
		}
		for _, e := range outbox {
			window := s.current().EventWindow()
			if e.Event.Kind == trace.ProgressKind {
				window = s.current().EventProgressWindow()
			}
			if e.Event.Operation == nil && !e.Acknowledged && (e.Claim != nil || !f.clock.Now().Before(e.At.Add(window))) {
				complete = false
				details = append(details, "pending event="+e.Event.ID)
			}
		}
		ops, err := repo.Operations(stream)
		if err != nil {
			complete = false
			details = append(details, fmt.Sprintf("operations: %v", err))
		}
		for _, op := range ops {
			if op.Result == nil {
				complete = false
				failure := ""
				if len(op.History) > 0 {
					failure = op.History[len(op.History)-1].Failure
				}
				details = append(details, fmt.Sprintf("operation=%s event=%s action=%s effect=%v retry=%s failure=%q", op.Operation.ID, op.EventID, op.Operation.Action, op.EffectStarted, op.RetryAt.Format(time.RFC3339Nano), failure))
			}
		}
	}
	for _, p := range projects {
		streams, err := p.repository.Workstreams()
		if err != nil {
			return false, fmt.Sprintf("%s streams: %v", p.id, err)
		}
		for _, stream := range streams {
			inspect(p.repository, stream)
		}
	}
	target, host, _, close, err := s.chiefRepository(context.Background(), projects[0].repository, f.stream1)
	if err != nil {
		return false, strings.Join(details, "; ") + fmt.Sprintf("; shared chief: %v", err)
	}
	inspect(target, host)
	close()
	return complete, strings.Join(details, "; ")
}

func (f *crossRepoInboxFixture) waitCompleted(t *testing.T, s *Service, label string) {
	t.Helper()
	f.driveTicksUntil(t, s, label, func() bool {
		complete, _ := f.snapshot(s)
		return complete
	})
}

func (f *crossRepoInboxFixture) problem(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.problems = append(f.problems, fmt.Sprintf(format, args...))
}

// repositories waits for both projects' own, now-live repository instances.
func (f *crossRepoInboxFixture) repositories(t *testing.T, s *Service) (repo1, repo2 *trace.Repository) {
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
			t.Fatalf("both projects' own repositories were never opened: %s", f.diagnostics(s))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// driveTicksUntil sends ticks, retrying on a short timeout, until condition
// holds or demoTimeout passes.
func (f *crossRepoInboxFixture) driveTicksUntil(t *testing.T, s *Service, label string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(demoTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
retry:
	for {
		checked := make(chan bool, 1)
		go func(out chan<- bool) { out <- condition() }(checked)
		// A blocked durable read must still reach the boundary diagnostics.
		// Only one condition read is in flight, and only the live controller
		// executes passes while we wait for it.
		for {
			select {
			case complete := <-checked:
				if complete {
					return
				}
				checked = nil
			case <-poll.C:
				if checked == nil {
					continue retry
				}
			case <-s.done:
				t.Fatalf("%s: %s", label, f.diagnostics(s))
			case f.ticks <- f.clock.Now():
			case <-deadline.C:
				t.Fatalf("%s did not complete: %s", label, f.diagnostics(s))
			}
		}
	}
}

// TestCrossRepoWorkgroupInboxConsolidatesAndRulingResumesOnlyAffectedAskers
// is unit wg-inbox's own evidence (discovery-36f3f2dfce65f7e4d5dcf323fe85d19a1a9d066d7c4d02028b99e95cc60a6dbf): a
// consolidated escalation spanning two repos' own members of one cross-repo
// workgroup appears exactly once in the owner's inbox, an unrelated
// escalation from each repo's own, separate one-workstream workgroup stays
// its own entry (each project's own inbox counter is independent of the
// workgroup's, and of the other project's), and the owner's ruling on the
// consolidated entry resumes all and only the two cross-repo askers, never
// the two unrelated ones.
func TestCrossRepoWorkgroupInboxConsolidatesAndRulingResumesOnlyAffectedAskers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const question1, question2 = "Where does the server store chunks?", "Does the client ever see the offset?"
	const questionU1, questionU2 = "May the index use a second file?", "Is the retry bounded?"
	const ruling = "Both are part of the contract."

	f := newCrossRepoInboxFixture(t)

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
	f.engine.turns["build_u1"] = ask(questionU1)
	f.engine.turns["build_u2"] = ask(questionU2)

	var flagMu sync.Mutex
	var escalated1, escalated2, escalatedU1, escalatedU2 bool
	var relayed bool
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		flagMu.Lock()
		switch {
		// Each cross-repo member's own question numbering is local to its
		// own workstream, so both questions of this consolidated batch are
		// locally numbered "1": the ruling notice reads "questions 1, 1".
		case !relayed && strings.Contains(req.Prompt, ", escalation_1 (questions 1, 1): "+ruling):
			flagMu.Unlock()
			out, err := callTool(ctx, tools, questions.RelayRulingTool, map[string]any{"question": string(f.stream1) + ":1", "text": ruling, "scope": "local"})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				f.problem("relay_ruling: %s %v", out, err)
			}
			flagMu.Lock()
			relayed = true
			flagMu.Unlock()
			return questionResult(req, "session-chief", "Relayed"), nil
		case !escalated1 && !escalated2 && strings.Contains(req.Prompt, "asked by the mason: "+question1) && strings.Contains(req.Prompt, "asked by the mason: "+question2):
			flagMu.Unlock()
			out, err := callTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{string(f.stream1) + ":1", string(f.stream2) + ":1"},
				"rephrasing": "Is the chunk store part of the contract?", "blocked": "Both workstreams.", "options": []string{"Yes", "No"}, "recommendation": "Yes."})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				f.problem("escalate consolidated: %s %v", out, err)
			}
			flagMu.Lock()
			escalated1, escalated2 = true, true
			flagMu.Unlock()
			return questionResult(req, "session-chief", "Escalated"), nil
		case !escalatedU1 && strings.Contains(req.Prompt, "asked by the mason: "+questionU1):
			flagMu.Unlock()
			out, err := callTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{"1"},
				"rephrasing": "May the index add a file?", "blocked": "The index unit.", "options": []string{}, "recommendation": "No."})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				f.problem("escalate u1: %s %v", out, err)
			}
			flagMu.Lock()
			escalatedU1 = true
			flagMu.Unlock()
			return questionResult(req, "session-chief", "Escalated"), nil
		case !escalatedU2 && strings.Contains(req.Prompt, "asked by the mason: "+questionU2):
			flagMu.Unlock()
			out, err := callTool(ctx, tools, questions.EscalateTool, map[string]any{"questions": []string{"1"},
				"rephrasing": "Is the retry bound owner-set?", "blocked": "The retry unit.", "options": []string{}, "recommendation": "No."})
			if err != nil || !strings.Contains(out, `"recorded":true`) {
				f.problem("escalate u2: %s %v", out, err)
			}
			flagMu.Lock()
			escalatedU2 = true
			flagMu.Unlock()
			return questionResult(req, "session-chief", "Escalated"), nil
		}
		flagMu.Unlock()
		if strings.Contains(req.Prompt, "asked by the mason:") || strings.Contains(req.Prompt, "escalation_1") {
			return questionResult(req, "session-chief", "Nothing further"), nil
		}
		f.problem("unexpected chief-of-staff turn:\n%s", req.Prompt)
		return questionResult(req, "session-chief", "Nothing to do"), nil
	}
	f.engine.turns[questions.TurnID("1")] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		return questionResult(req, "session-"+req.Name, "Continued"), nil
	}

	s, err := Start(ctx, f.cfg)
	must(t, err)
	f.observePasses(s)
	closed := false
	defer func() {
		if !closed {
			s.Close()
		}
	}()
	repo1, repo2 := f.repositories(t, s)
	c := NewClient(s.Socket())
	defer c.Close()

	id, api := s.DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, f.stream2})
	if api != nil {
		t.Fatalf("draft workgroup: %v", api)
	}
	workgroupProject := config.ProjectID("p_" + strings.TrimPrefix(string(id), "g_"))

	// No asking turn may run while DraftWorkgroup retracts the old chiefs
	// and records member pointers. Start the live workers only after that
	// join is complete; the live controllers remain their sole pass driver.
	for _, m := range []struct {
		repo                *trace.Repository
		stream              config.WorkstreamID
		agent, thread, turn string
	}{
		{repo1, f.stream1, demoAgent, demoThread, "build1"},
		{repo2, f.stream2, "agent_mason2", "thread_mason2", "build2"},
		{repo1, unrelatedStream1, "agent_u1", "thread_u1", "build_u1"},
		{repo2, unrelatedStream2, "agent_u2", "thread_u2", "build_u2"},
	} {
		h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + m.turn, Revision: 1, Project: m.repo.Project(), Workstream: m.stream, At: f.clock.Now(), Actor: ownerActor, Cause: "message_" + m.turn, Depth: 1}
		_, err := m.repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: m.agent, ThreadID: m.thread, TurnID: m.turn,
			Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, SystemPrompt: "You are the mason.", Prompt: "Work: " + m.turn})
		must(t, err)
	}

	// Each asker's own question notice is delivered to its chief of staff
	// only once its event window closes.
	f.waitCompleted(t, s, "first asking turns after the completed join")
	for _, m := range []struct {
		repo   *trace.Repository
		stream config.WorkstreamID
		agent  string
	}{
		{repo1, f.stream1, demoAgent}, {repo2, f.stream2, "agent_mason2"},
		{repo1, unrelatedStream1, "agent_u1"}, {repo2, unrelatedStream2, "agent_u2"},
	} {
		th, err := m.repo.Thread(m.stream, m.agent)
		must(t, err)
		if len(th.Turns) != 1 || !th.Parked() || th.Turns[0].Status() != "waiting" {
			t.Fatalf("first asking turn %s/%s: %+v; %s", m.stream, m.agent, th, f.diagnostics(s))
		}
	}
	f.clock.Advance(time.Minute)
	f.driveTicksUntil(t, s, "consolidated and unrelated escalations", func() bool {
		flagMu.Lock()
		defer flagMu.Unlock()
		return escalated1 && escalated2 && escalatedU1 && escalatedU2
	})
	f.waitCompleted(t, s, "escalations and event acknowledgements")

	inbox, err := c.Inbox(ctx)
	must(t, err)
	if len(inbox.Entries) != 3 {
		t.Fatalf("inbox before any ruling: %+v", inbox)
	}
	var consolidated, entryU1, entryU2 InboxEntry
	for _, e := range inbox.Entries {
		switch e.Project {
		case workgroupProject:
			consolidated = e
		case project:
			entryU1 = e
		case otherProject:
			entryU2 = e
		default:
			t.Fatalf("entry with an unexpected project %s: %+v", e.Project, e)
		}
	}
	if consolidated.Number == 0 {
		t.Fatalf("no consolidated entry tagged with the workgroup's own project %s: %+v", workgroupProject, inbox)
	}
	// The consolidated escalation spans both members, in one entry, with
	// both questions.
	if consolidated.Batch != "escalation_1" || len(consolidated.Asked) != 2 {
		t.Fatalf("consolidated entry: %+v", consolidated)
	}
	gotQuestions := []string{consolidated.Asked[0].Question, consolidated.Asked[1].Question}
	wantQuestions := []string{question1, question2}
	if !(reflect.DeepEqual(gotQuestions, wantQuestions) || reflect.DeepEqual(gotQuestions, []string{question2, question1})) {
		t.Fatalf("consolidated entry's questions: %v", gotQuestions)
	}
	// Each project's own unrelated escalation is its own entry, with its own
	// project's inbox number: project's own counter and otherProject's own
	// counter, and the workgroup's own counter, each start at 1
	// independently, and the three entries are never merged or confused,
	// because their Project differs.
	if entryU1.Number != 1 || entryU1.Batch != "escalation_1" || entryU1.Question != "May the index add a file?" {
		t.Fatalf("project's own unrelated entry: %+v", entryU1)
	}
	if entryU2.Number != 1 || entryU2.Batch != "escalation_1" || entryU2.Question != "Is the retry bound owner-set?" {
		t.Fatalf("otherProject's own unrelated entry: %+v", entryU2)
	}
	if consolidated.Number != 1 {
		t.Fatalf("workgroup's own first escalation should also start its own counter at 1, got %d", consolidated.Number)
	}

	// The owner rules on the consolidated entry alone. Both unrelated
	// entries are untouched by it.
	_, err = c.Answer(ctx, consolidated.Number, ruling, workgroupProject)
	must(t, err)
	inbox, err = c.Inbox(ctx)
	must(t, err)
	if len(inbox.Entries) != 2 {
		t.Fatalf("inbox after ruling on the consolidated entry: %+v", inbox)
	}
	for _, e := range inbox.Entries {
		if e.Project == workgroupProject {
			t.Fatalf("the consolidated entry is still open after its ruling: %+v", e)
		}
	}

	f.clock.Advance(time.Minute)
	f.driveTicksUntil(t, s, "owner ruling relay", func() bool {
		flagMu.Lock()
		defer flagMu.Unlock()
		return relayed
	})

	resumedWith := func(repo *trace.Repository, stream config.WorkstreamID, agentID string) bool {
		th, err := repo.Thread(stream, agentID)
		if err != nil {
			return false
		}
		i := -1
		for idx, q := range th.Turns {
			if q.Request.TurnID == questions.TurnID("1") {
				i = idx
			}
		}
		return len(th.Turns) == 2 && i == 1 && strings.Contains(th.Turns[i].Request.Prompt, ruling) &&
			!th.Parked() && !th.Turns[i].CompletedAt.IsZero() && th.Turns[i].Response != nil && th.Turns[i].Status() == "idle"
	}
	f.driveTicksUntil(t, s, "both affected workers complete exactly one continuation", func() bool {
		return resumedWith(repo1, f.stream1, demoAgent) && resumedWith(repo2, f.stream2, "agent_mason2")
	})
	f.waitCompleted(t, s, "ruling continuations and event acknowledgements")

	// Neither unrelated asker was ever resumed: the consolidated ruling
	// affected only the two cross-repo askers it named.
	notResumed := func(repo *trace.Repository, stream config.WorkstreamID, agentID string) bool {
		th, err := repo.Thread(stream, agentID)
		if err != nil {
			return false
		}
		for _, q := range th.Turns {
			if q.Request.TurnID == questions.TurnID("1") {
				return false
			}
		}
		return len(th.Turns) == 1
	}
	if !notResumed(repo1, unrelatedStream1, "agent_u1") {
		t.Fatalf("project's own unrelated asker was resumed by the cross-repo ruling: %+v", must2(repo1.Thread(unrelatedStream1, "agent_u1")))
	}
	if !notResumed(repo2, unrelatedStream2, "agent_u2") {
		t.Fatalf("otherProject's own unrelated asker was resumed by the cross-repo ruling: %+v", must2(repo2.Thread(unrelatedStream2, "agent_u2")))
	}

	// Each project's own unrelated entry still carries its own project's
	// inbox number, unaffected by the workgroup's ruling and by the other
	// project's own entry.
	inbox, err = c.Inbox(ctx)
	must(t, err)
	if len(inbox.Entries) != 2 {
		t.Fatalf("inbox after the relay: %+v", inbox)
	}
	for _, e := range inbox.Entries {
		if e.Number != 1 || (e.Project != project && e.Project != otherProject) {
			t.Fatalf("unrelated entry after the relay: %+v", e)
		}
	}

	// Freeze the durable thread and owner views after completed delivery,
	// then restart the real service. Startup must neither rewrap members
	// nor redeliver the consolidated escalation or either worker answer.
	threadsSnapshot := func() []byte {
		t.Helper()
		var threads []trace.Thread
		for _, m := range []struct {
			repo   *trace.Repository
			stream config.WorkstreamID
			agent  string
		}{
			{repo1, f.stream1, demoAgent}, {repo2, f.stream2, "agent_mason2"},
			{repo1, unrelatedStream1, "agent_u1"}, {repo2, unrelatedStream2, "agent_u2"},
			{repo1, unrelatedStream1, trace.ChiefOfStaff}, {repo2, unrelatedStream2, trace.ChiefOfStaff},
		} {
			th, err := m.repo.Thread(m.stream, m.agent)
			must(t, err)
			threads = append(threads, th)
		}
		target, host, _, close, err := s.chiefRepository(ctx, repo1, f.stream1)
		must(t, err)
		defer close()
		th, err := target.ChiefOfStaffThread(host)
		must(t, err)
		threads = append(threads, th)
		data, err := json.Marshal(threads)
		must(t, err)
		return data
	}
	beforeThreads := threadsSnapshot()
	beforeConversation1, err := c.Conversation(ctx, f.stream1)
	must(t, err)
	beforeConversation2, err := c.Conversation(ctx, f.stream2)
	must(t, err)
	c.Close()
	must(t, s.Close())
	closed = true
	s, err = Start(ctx, f.cfg)
	must(t, err)
	f.observePasses(s)
	closed = false
	c = NewClient(s.Socket())
	defer c.Close()
	repo1, repo2 = f.repositories(t, s)
	f.waitCompleted(t, s, "completed state after restart")
	afterThreads := threadsSnapshot()
	if string(afterThreads) != string(beforeThreads) {
		t.Fatalf("restart changed durable worker or chief threads: before=%s after=%s", beforeThreads, afterThreads)
	}
	afterInbox, err := c.Inbox(ctx)
	must(t, err)
	if !reflect.DeepEqual(afterInbox, inbox) {
		t.Fatalf("restart changed the unaffected inbox or repeated the consolidated escalation: before=%+v after=%+v", inbox, afterInbox)
	}
	afterConversation1, err := c.Conversation(ctx, f.stream1)
	must(t, err)
	afterConversation2, err := c.Conversation(ctx, f.stream2)
	must(t, err)
	if !reflect.DeepEqual(afterConversation1, beforeConversation1) || !reflect.DeepEqual(afterConversation2, beforeConversation2) {
		t.Fatalf("restart changed member conversations: before=%+v / %+v after=%+v / %+v", beforeConversation1, beforeConversation2, afterConversation1, afterConversation2)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.problems) != 0 {
		t.Fatal(strings.Join(f.problems, "\n"))
	}
}

// must2 returns v for use inside a Fatalf argument list, discarding err; the
// call site has already failed and only wants v for the failure message.
func must2[T any](v T, _ error) T { return v }
