package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type priorityRig struct {
	t         *testing.T
	ctx       context.Context
	opts      Options
	s         *Service
	c         *Client
	delivered config.WorkstreamID
	gone      config.WorkstreamID
	unknown   config.WorkstreamID
	sends     int

	mu       sync.Mutex
	problems []string
	results  map[string][]string
	prompts  map[string]string
	listed   []*mcp.Tool
}

const (
	priorityInvalidPrompt = "Put quiet first, then the rest."
	priorityValidPrompt   = "Put quiet first, then the upload work."
	priorityPausePrompt   = "Pause all work for travel."
	priorityResumePrompt  = "Resume all work."
)

func newPriorityRig(t *testing.T) *priorityRig {
	t.Helper()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "pr-")
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString("[roles.manager]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
	fixturekit.Must(t, errors.Join(err, configFile.Close()))

	r := &priorityRig{t: t, ctx: ctx, results: map[string][]string{}, prompts: map[string]string{},
		delivered: "w_1111111111111111111111111111111d", gone: "w_2222222222222222222222222222222a", unknown: "w_3333333333333333333333333333333f"}
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	for w, state := range map[config.WorkstreamID]string{r.delivered: DeliveredState, r.gone: AbandonedState} {
		fixturekit.Must(t, repo.CreateWorkstream(ctx, w, fixturekit.DemoStart, ownerActor))
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "end", Revision: 1, Project: fixturekit.Project, Workstream: w, At: fixturekit.DemoStart, Actor: ownerActor, Cause: "end"}
		_, err := repo.SetFeatureState(ctx, h, state, "ended")
		fixturekit.Must(t, err)
	}
	_, err = repo.EnsureManager(ctx, fixturekit.Stream, fixturekit.DemoStart, serviceActor)
	fixturekit.Must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{
		Header:  trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_events_seed", Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: serviceActor, Cause: "events"},
		AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "events_seed", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"},
		SystemPrompt: fmt.Sprintf(followUpPrompt, fixturekit.Stream) + "\n\n" + managerGuidance(), Prompt: "A service event."})
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Close())

	sessions := &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &fixturekit.DemoEngine{Sessions: sessions, Turns: map[string]fixturekit.DemoTurn{}}
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	r.opts = Enforce(opts, Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}}})

	seeded := make(chan struct{})
	engine.Turns["events_seed"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		defer close(seeded)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.prioritise(ctx, session, "event", []string{string(intakefixture.Quiet), string(fixturekit.Stream)})
		out, err := fixturekit.CallTool(ctx, session, pauseTool, map[string]any{"scope": "factory", "reason": "travel"})
		if err != nil || !strings.Contains(out, "only an owner message") {
			r.problems = append(r.problems, fmt.Sprintf("event pause: %s %v", out, err))
		}
		for name, input := range map[string]map[string]any{
			requestChangeTool: {"key": "event-change", "kind": "cancel", "scope": "workgroup", "change": "Cancel without an owner request"},
			decideChangeTool:  {"id": "event-change", "revision": 1, "decision": "approve", "pull_requests": "close"},
		} {
			out, err := fixturekit.CallTool(ctx, session, name, input)
			if err != nil || !strings.Contains(out, "only an owner message") {
				r.problems = append(r.problems, fmt.Sprintf("event %s: %s %v", name, out, err))
			}
		}
		return intakefixture.QuestionResult(req, "session-chief", "Nothing to do"), nil
	}
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, "events_") {
			return intakefixture.QuestionResult(req, "session-events", "Noted"), nil
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		own := intakefixture.OwnPrompt(req.Prompt)
		r.prompts[own] = req.SystemPrompt
		all, err := session.ListTools(ctx, nil)
		if err != nil {
			return nil, err
		}
		r.listed = all.Tools
		switch own {
		case priorityInvalidPrompt:
			for _, order := range [][]config.WorkstreamID{{}, {"quiet"}, {r.unknown}, {intakefixture.Quiet, intakefixture.Quiet}, {intakefixture.Quiet, r.delivered}, {r.gone}} {
				ids := []string{}
				for _, w := range order {
					ids = append(ids, string(w))
				}
				r.prioritise(ctx, session, "invalid", ids)
			}
		case priorityValidPrompt:
			r.prioritise(ctx, session, "valid", []string{string(intakefixture.Quiet), string(fixturekit.Stream)})
		case priorityPausePrompt:
			out, err := fixturekit.CallTool(ctx, session, pauseTool, map[string]any{"scope": "factory", "reason": "travel"})
			if err != nil || out != `{"recorded":true}` {
				r.problems = append(r.problems, fmt.Sprintf("owner pause: %s %v", out, err))
			}
		case priorityResumePrompt:
			out, err := fixturekit.CallTool(ctx, session, resumeTool, map[string]any{"scope": "factory"})
			if err != nil || out != `{"recorded":true}` {
				r.problems = append(r.problems, fmt.Sprintf("owner resume: %s %v", out, err))
			}
		default:
			r.problems = append(r.problems, "unexpected turn "+req.Name)
		}
		return intakefixture.QuestionResult(req, "session-chief", "Done"), nil
	}

	r.s, r.c = start(t, r.opts)
	select {
	case <-seeded:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the service's manager turn did not run")
	}
	intakefixture.Mutation(t, r.c, "PUT", "priority", PriorityRequest{Project: fixturekit.Project, Workstreams: []config.WorkstreamID{fixturekit.Stream}})
	return r
}

func (r *priorityRig) prioritise(ctx context.Context, session *mcp.ClientSession, step string, order []string) {
	out, err := fixturekit.CallTool(ctx, session, prioritiseTool, map[string]any{"workstreams": order})
	if err != nil {
		r.problems = append(r.problems, fmt.Sprint(step, ": ", err))
	}
	r.results[step] = append(r.results[step], out)
}

func (r *priorityRig) send(prompt string) ConversationEntry {
	r.t.Helper()
	sent, err := r.c.Send(r.ctx, fixturekit.Stream, prompt)
	fixturekit.Must(r.t, err)
	r.sends++
	intakefixture.AwaitConversation(r.t, r.c, func(l ConversationResponse) bool {
		return len(l.Entries) == 2*r.sends && l.Entries[len(l.Entries)-1].State == TurnDone
	})
	return sent
}

func (r *priorityRig) requireNoProblems() {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.problems) > 0 {
		r.t.Fatal(strings.Join(r.problems, "\n"))
	}
}

func (r *priorityRig) order() []config.WorkstreamID {
	r.t.Helper()
	rt, err := r.c.Runtime(r.ctx)
	fixturekit.Must(r.t, err)
	for _, p := range rt.Effective.Priorities {
		if p.Project == fixturekit.Project {
			return p.Workstreams
		}
	}
	return nil
}

func (r *priorityRig) pauses() []runtime.Pause {
	r.t.Helper()
	rt, err := r.c.Runtime(r.ctx)
	fixturekit.Must(r.t, err)
	return rt.Effective.Pauses
}

func (r *priorityRig) changes() []trace.PriorityChange {
	r.t.Helper()
	out, err := trace.Read[trace.PriorityChange](r.s.sole().repository, fixturekit.Stream)
	fixturekit.Must(r.t, err)
	return out
}

func (r *priorityRig) requireRecordedFor(sent ConversationEntry, want []config.WorkstreamID) {
	r.t.Helper()
	got := r.changes()
	request := "request_" + strings.TrimPrefix(sent.Turn, "message_")
	if len(got) != 1 {
		r.t.Fatalf("priority records: %+v", got)
	}
	p := got[0]
	if p.ID != trace.PriorityID || p.Revision != 1 || p.Actor != ownerActor || p.Cause != request || p.Agent != trace.Manager || p.Turn != sent.Turn || !slices.Equal(p.Order, want) {
		r.t.Fatalf("priority record %+v", p)
	}
}

func (r *priorityRig) requireOwnerPause() {
	r.t.Helper()
	pauses := r.pauses()
	if len(pauses) != 1 || pauses[0].Source != runtime.PauseOwner || pauses[0].Reason != "travel" || pauses[0].SetAt.IsZero() {
		r.t.Fatalf("chief pause: %+v", pauses)
	}
}

func TestManagerEventTurnsCannotActForTheOwner(t *testing.T) {
	t.Parallel()
	r := newPriorityRig(t)
	r.requireNoProblems()
	want := `{"recorded":false,"reason":"only the owner can change the priority order; this turn does not answer a message from the owner"}`
	if got := r.results["event"]; !slices.Equal(got, []string{want}) {
		t.Fatalf("event turn priority result %v, want %s", got, want)
	}
	if got := r.pauses(); len(got) != 0 {
		t.Fatalf("an event turn paused the factory: %+v", got)
	}
}

func TestManagerRefusesInvalidPriorityOrders(t *testing.T) {
	t.Parallel()
	r := newPriorityRig(t)
	r.send(priorityInvalidPrompt)
	r.requireNoProblems()
	want := []string{
		`{"recorded":false,"reason":"workstreams must name at least one active workstream"}`,
		`{"recorded":false,"reason":"\"quiet\" is not a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}`,
		`{"recorded":false,"reason":"workstream ` + string(r.unknown) + ` is not a workstream of this project"}`,
		`{"recorded":false,"reason":"workstream ` + string(intakefixture.Quiet) + ` is listed more than once"}`,
		`{"recorded":false,"reason":"workstream ` + string(r.delivered) + ` is delivered; only active workstreams have a priority"}`,
		`{"recorded":false,"reason":"workstream ` + string(r.gone) + ` is abandoned; only active workstreams have a priority"}`,
	}
	if got := r.results["invalid"]; !slices.Equal(got, want) {
		t.Fatalf("tool results:\n%v\nwant:\n%v", got, want)
	}
	if got := r.order(); !slices.Equal(got, []config.WorkstreamID{fixturekit.Stream}) {
		t.Fatalf("refused orders changed the priority to %v", got)
	}
	if got := r.changes(); len(got) != 0 {
		t.Fatalf("refused orders were recorded: %+v", got)
	}
}

func TestManagerSetsPriorityOrderForTheOwner(t *testing.T) {
	t.Parallel()
	r := newPriorityRig(t)
	sent := r.send(priorityValidPrompt)
	r.requireNoProblems()
	want := []config.WorkstreamID{intakefixture.Quiet, fixturekit.Stream}
	if got := r.results["valid"]; !slices.Equal(got, []string{`{"recorded":true,"workstreams":["` + string(intakefixture.Quiet) + `","` + string(fixturekit.Stream) + `"]}`}) {
		t.Fatalf("tool results: %v", got)
	}
	if got := r.order(); !slices.Equal(got, want) {
		t.Fatalf("runtime priority %v, want %v", got, want)
	}
	r.requireRecordedFor(sent, want)
}

func TestManagerPausesTheFactoryForTheOwner(t *testing.T) {
	t.Parallel()
	r := newPriorityRig(t)
	r.send(priorityPausePrompt)
	r.requireNoProblems()
	r.requireOwnerPause()
}

func TestManagerResumesTheFactoryForTheOwner(t *testing.T) {
	t.Parallel()
	r := newPriorityRig(t)
	r.send(priorityPausePrompt)
	r.send(priorityResumePrompt)
	r.requireNoProblems()
	if got := r.pauses(); len(got) != 0 {
		t.Fatalf("chief resume: %+v", got)
	}
}

func TestOwnerTurnsOfTheManagerHaveTheManagerToolsAndGuidance(t *testing.T) {
	t.Parallel()
	r := newPriorityRig(t)
	r.send(priorityValidPrompt)
	r.requireNoProblems()
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for _, tool := range r.listed {
		names = append(names, tool.Name)
		if tool.Name == prioritiseTool && (!strings.Contains(tool.Description, "when the owner asks for it") || !strings.Contains(tool.Description, "highest priority first")) {
			t.Fatalf("prioritise description: %s", tool.Description)
		}
	}
	slices.Sort(names)
	if wantNames := []string{"answer", "ask_owner_about_checks", "capacity", "decide", "decide_amendment", "decide_change", "decide_charter", "drop_dependency", "escalate", "factory_context", "file_read", "flake_fix", "follow_up", "hand_back_drift", "inspect_code", "log_assumption", "move_unit", "move_workstream", "notify", "pause", "preview", "prioritise", "project_context", "propose_charter", "pull_request_checks", "read_cloud_trace", "read_remote_file", "record_delivery_brief", "record_discovery", "relay_ruling", "request_change", "resolve_contested", "resolve_objection", "resume", "retry_change", "route_amendment", "set_status", "triage_finding"}; !slices.Equal(names, wantNames) {
		t.Fatalf("manager tools %v, want %v", names, wantNames)
	}
	for prompt, system := range r.prompts {
		if !strings.Contains(system, priorityGuidance) || !strings.Contains(system, amendmentGuidance) {
			t.Fatalf("owner turn %q lacks the priority or amendment guidance:\n%s", prompt, system)
		}
	}
}

func TestPriorityOrderAndManagerPauseSurviveARestart(t *testing.T) {
	t.Parallel()
	r := newPriorityRig(t)
	sent := r.send(priorityValidPrompt)
	r.send(priorityPausePrompt)
	r.requireNoProblems()
	want := []config.WorkstreamID{intakefixture.Quiet, fixturekit.Stream}
	paused := r.pauses()

	r.c.Close()
	fixturekit.Must(t, r.s.Close())
	r.s, r.c = start(t, r.opts)
	if got := r.order(); !slices.Equal(got, want) {
		t.Fatalf("runtime priority after restart %v, want %v", got, want)
	}
	if got := r.pauses(); !reflect.DeepEqual(got, paused) {
		t.Fatalf("chief pause after restart: %+v", got)
	}
	r.requireRecordedFor(sent, want)
	st, _ := r.s.store.Effective()
	if !reflect.DeepEqual(st.Priorities, []runtime.Priority{{Project: fixturekit.Project, Workstreams: want}}) {
		t.Fatalf("scheduler priorities %+v", st.Priorities)
	}
}
