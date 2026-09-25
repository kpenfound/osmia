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

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// The chief of staff sets the project's priority order when the owner asks,
// through the production tools: a turn the owner did not ask for and every
// invalid order change nothing, and a valid order is the one the runtime API
// reports and the trace records, across a restart.
func TestChiefOfStaffSetsPriorityForTheOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "pr-")
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
	must(t, errors.Join(err, configFile.Close()))

	delivered := config.WorkstreamID("w_1111111111111111111111111111111d")
	gone := config.WorkstreamID("w_2222222222222222222222222222222a")
	unknown := config.WorkstreamID("w_3333333333333333333333333333333f")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	for w, state := range map[config.WorkstreamID]string{delivered: DeliveredState, gone: AbandonedState} {
		must(t, repo.CreateWorkstream(ctx, w, demoStart, ownerActor))
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "end", Revision: 1, Project: project, Workstream: w, At: demoStart, Actor: ownerActor, Cause: "end"}
		_, err := repo.SetFeatureState(ctx, h, state, "ended")
		must(t, err)
	}
	// A turn of the chief of staff that the service, not the owner, asked for.
	_, err = repo.EnsureChiefOfStaff(ctx, stream, demoStart, serviceActor)
	must(t, err)
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{
		Header:  trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_events_seed", Revision: 1, Project: project, Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "events"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "events_seed", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"},
		SystemPrompt: "You are the chief of staff.", Prompt: "A service event."})
	must(t, err)
	must(t, repo.Close())

	sessions := &demoSessions{byKey: map[string]*mcp.ClientSession{}}
	engine := &demoEngine{sessions: sessions, turns: map[string]demoTurn{}}
	engine.resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	opts = Enforce(opts, Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &demoTransport{sessions: sessions}}})

	var mu sync.Mutex
	var problems []string
	results := map[string][]string{}
	prompts := map[string]string{}
	var listed []*mcp.Tool
	seeded := make(chan struct{})
	prioritise := func(ctx context.Context, session *mcp.ClientSession, step string, order []string) {
		out, err := callTool(ctx, session, prioritiseTool, map[string]any{"workstreams": order})
		if err != nil {
			problems = append(problems, fmt.Sprint(step, ": ", err))
		}
		results[step] = append(results[step], out)
	}
	engine.turns["events_seed"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		defer close(seeded)
		mu.Lock()
		defer mu.Unlock()
		prioritise(ctx, session, "event", []string{string(quiet), string(stream)})
		out, err := callTool(ctx, session, pauseTool, map[string]any{"scope": "factory", "reason": "travel"})
		if err != nil || !strings.Contains(out, "only an owner message") {
			problems = append(problems, fmt.Sprintf("event pause: %s %v", out, err))
		}
		return questionResult(req, "session-chief", "Nothing to do"), nil
	}
	engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, session *mcp.ClientSession) (*agent.Result, error) {
		// Ending the seeded workstreams delivers a notice to the delivered
		// one's chief of staff.
		if strings.HasPrefix(req.Name, "events_") {
			return questionResult(req, "session-events", "Noted"), nil
		}
		mu.Lock()
		defer mu.Unlock()
		prompts[req.Prompt] = req.SystemPrompt
		all, err := session.ListTools(ctx, nil)
		if err != nil {
			return nil, err
		}
		listed = all.Tools
		switch req.Prompt {
		case "Put quiet first, then the rest.":
			for _, order := range [][]config.WorkstreamID{{}, {"quiet"}, {unknown}, {quiet, quiet}, {quiet, delivered}, {gone}} {
				ids := []string{}
				for _, w := range order {
					ids = append(ids, string(w))
				}
				prioritise(ctx, session, "invalid", ids)
			}
		case "Put quiet first, then the upload work.":
			prioritise(ctx, session, "valid", []string{string(quiet), string(stream)})
			out, err := callTool(ctx, session, pauseTool, map[string]any{"scope": "factory", "reason": "travel"})
			if err != nil || out != `{"recorded":true}` {
				problems = append(problems, fmt.Sprintf("owner pause: %s %v", out, err))
			}
		case "Resume all work.":
			out, err := callTool(ctx, session, resumeTool, map[string]any{"scope": "factory"})
			if err != nil || out != `{"recorded":true}` {
				problems = append(problems, fmt.Sprintf("owner resume: %s %v", out, err))
			}
		default:
			problems = append(problems, "unexpected turn "+req.Name)
		}
		return questionResult(req, "session-chief", "Done"), nil
	}

	s, c := start(t, opts)
	select {
	case <-seeded:
	case <-time.After(demoTimeout):
		t.Fatal("the service's chief-of-staff turn did not run")
	}
	// The owner's order through the runtime API.
	mutation(t, c, "PUT", "priority", PriorityRequest{Project: project, Workstreams: []config.WorkstreamID{stream}})
	order := func(c *Client) []config.WorkstreamID {
		t.Helper()
		rt, err := c.Runtime(ctx)
		must(t, err)
		for _, p := range rt.Effective.Priorities {
			if p.Project == project {
				return p.Workstreams
			}
		}
		return nil
	}
	changes := func(r *trace.Repository) []trace.PriorityChange {
		t.Helper()
		out, err := trace.Read[trace.PriorityChange](r, stream)
		must(t, err)
		return out
	}

	_, err = c.Send(ctx, stream, "Put quiet first, then the rest.")
	must(t, err)
	awaitConversation(t, c, func(l ConversationResponse) bool { return len(l.Entries) == 2 && l.Entries[1].State == TurnDone })
	if got := order(c); !slices.Equal(got, []config.WorkstreamID{stream}) {
		t.Fatalf("refused orders changed the priority to %v", got)
	}
	if got := changes(s.active.repository); len(got) != 0 {
		t.Fatalf("refused orders were recorded: %+v", got)
	}

	sent, err := c.Send(ctx, stream, "Put quiet first, then the upload work.")
	must(t, err)
	awaitConversation(t, c, func(l ConversationResponse) bool { return len(l.Entries) == 4 && l.Entries[3].State == TurnDone })
	want := []config.WorkstreamID{quiet, stream}
	if got := order(c); !slices.Equal(got, want) {
		t.Fatalf("runtime priority %v, want %v", got, want)
	}
	paused, err := c.Runtime(ctx)
	must(t, err)
	if len(paused.Effective.Pauses) != 1 || paused.Effective.Pauses[0].Source != runtime.PauseOwner || paused.Effective.Pauses[0].Reason != "travel" || paused.Effective.Pauses[0].SetAt.IsZero() {
		t.Fatalf("chief pause: %+v", paused.Effective.Pauses)
	}

	mu.Lock()
	if len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
	wantResults := map[string][]string{
		"event": {`{"recorded":false,"reason":"only the owner can change the priority order; this turn does not answer a message from the owner"}`},
		"invalid": {
			`{"recorded":false,"reason":"workstreams must name at least one active workstream"}`,
			`{"recorded":false,"reason":"\"quiet\" is not a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}`,
			`{"recorded":false,"reason":"workstream ` + string(unknown) + ` is not a workstream of this project"}`,
			`{"recorded":false,"reason":"workstream ` + string(quiet) + ` is listed more than once"}`,
			`{"recorded":false,"reason":"workstream ` + string(delivered) + ` is delivered; only active workstreams have a priority"}`,
			`{"recorded":false,"reason":"workstream ` + string(gone) + ` is abandoned; only active workstreams have a priority"}`,
		},
		"valid": {`{"recorded":true,"workstreams":["` + string(quiet) + `","` + string(stream) + `"]}`},
	}
	if !reflect.DeepEqual(results, wantResults) {
		t.Fatalf("tool results:\n%v\nwant:\n%v", results, wantResults)
	}
	var names []string
	for _, tool := range listed {
		names = append(names, tool.Name)
		if tool.Name == prioritiseTool && (!strings.Contains(tool.Description, "when the owner asks for it") || !strings.Contains(tool.Description, "highest priority first")) {
			t.Fatalf("prioritise description: %s", tool.Description)
		}
	}
	slices.Sort(names)
	if wantNames := []string{"answer", "decide_amendment", "decide_charter", "escalate", "pause", "prioritise", "propose_charter", "relay_ruling", "resume", "route_amendment", "set_status"}; !slices.Equal(names, wantNames) {
		t.Fatalf("chief-of-staff tools %v, want %v", names, wantNames)
	}
	for prompt, system := range prompts {
		if !strings.Contains(system, priorityGuidance) || !strings.Contains(system, amendmentGuidance) {
			t.Fatalf("owner turn %q lacks the priority or amendment guidance:\n%s", prompt, system)
		}
	}
	mu.Unlock()

	check := func(got []trace.PriorityChange) {
		t.Helper()
		request := "request_" + strings.TrimPrefix(sent.Turn, "message_")
		if len(got) != 1 {
			t.Fatalf("priority records: %+v", got)
		}
		p := got[0]
		if p.ID != trace.PriorityID || p.Revision != 1 || p.Actor != ownerActor || p.Cause != request || p.Agent != trace.ChiefOfStaff || p.Turn != sent.Turn || !slices.Equal(p.Order, want) {
			t.Fatalf("priority record %+v", p)
		}
	}
	check(changes(s.active.repository))

	// A new service lifetime reads the same order and the same record.
	c.Close()
	must(t, s.Close())
	s, c = start(t, opts)
	if got := order(c); !slices.Equal(got, want) {
		t.Fatalf("runtime priority after restart %v, want %v", got, want)
	}
	reopened, err := c.Runtime(ctx)
	must(t, err)
	if !reflect.DeepEqual(reopened.Effective.Pauses, paused.Effective.Pauses) {
		t.Fatalf("chief pause after restart: %+v", reopened.Effective.Pauses)
	}
	check(changes(s.active.repository))
	st, _ := s.store.Effective()
	if !reflect.DeepEqual(st.Priorities, []runtime.Priority{{Project: project, Workstreams: want}}) {
		t.Fatalf("scheduler priorities %+v", st.Priorities)
	}
	_, err = c.Send(ctx, stream, "Resume all work.")
	must(t, err)
	awaitConversation(t, c, func(l ConversationResponse) bool { return len(l.Entries) == 6 && l.Entries[5].State == TurnDone })
	resumed, err := c.Runtime(ctx)
	must(t, err)
	if len(resumed.Effective.Pauses) != 0 {
		t.Fatalf("chief resume: %+v", resumed.Effective.Pauses)
	}
}
