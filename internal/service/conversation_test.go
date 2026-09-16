package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// chiefTurns is a fake chief of staff. It answers each prompt, and the first
// turn it runs waits for hold to close when hold is set.
type chiefTurns struct {
	mu      sync.Mutex
	calls   []coreadapter.PreparedTurn
	hold    chan struct{}
	entered chan struct{}
}

func (c *chiefTurns) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	c.mu.Lock()
	c.calls = append(c.calls, p)
	first := len(c.calls) == 1
	c.mu.Unlock()
	if first && c.hold != nil {
		close(c.entered)
		select {
		case <-c.hold:
		case <-ctx.Done():
			return coreadapter.SessionResult{}, ctx.Err()
		}
	}
	session := coreadapter.BackendSession{Backend: "claude", ID: "session"}
	switch p.Prompt {
	case "fail":
		return coreadapter.SessionResult{Session: session, FinalResponse: "partial", IsError: true}, nil
	case "cancel":
		return coreadapter.SessionResult{Session: session, FinalResponse: "stopped", Cancelled: true}, nil
	case "silent":
		return coreadapter.SessionResult{Session: session}, nil
	}
	return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "session"}, FinalResponse: "Answer to " + p.Prompt}, nil
}

func (c *chiefTurns) Calls() []coreadapter.PreparedTurn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]coreadapter.PreparedTurn(nil), c.calls...)
}

// conversationFixture creates a project with a trace holding two workstreams
// and a charter rule.
func conversationFixture(t *testing.T, prefix string) (Options, *config.Config) {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", prefix)
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := fixtureAt(t, home)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	demoGit(t, home, "-C", cfg.Project.Clone, "init", "-q")
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, demoStart, owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, demoStart, owner))
	must(t, repo.CreateWorkstream(ctx, quiet, demoStart, owner))
	must(t, repo.Close())
	dir, err := cfg.Root.ProjectTrace(project)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "charter.md"), []byte(trace.CharterTemplate+"\n## Rules\n\n1. Keep the upload API stable.\n"), 0600))
	opts.Reconciliation.Now = (&demoClock{now: demoStart}).Now
	return opts, cfg
}

// runChief binds the fake chief of staff through the thread dispatcher.
func runChief(opts *Options, cfg *config.Config, turns *chiefTurns) {
	opts.Threads = func(r *trace.Repository) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: opts.Reconciliation.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(cfg.Root.String(), "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
}

// awaitConversation polls the listing until done accepts it.
func awaitConversation(t *testing.T, c *Client, done func(ConversationResponse) bool) ConversationResponse {
	t.Helper()
	deadline := time.Now().Add(demoTimeout)
	for {
		list, err := c.Conversation(context.Background(), stream)
		must(t, err)
		if done(list) {
			return list
		}
		if time.Now().After(deadline) {
			t.Fatalf("conversation did not settle: %+v", list)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func states(list ConversationResponse) []string {
	var out []string
	for _, e := range list.Entries {
		out = append(out, e.Kind+":"+string(e.State))
	}
	return out
}

func TestConversationRunsMessagesInOrder(t *testing.T) {
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "cv-")
	turns := &chiefTurns{hold: make(chan struct{}), entered: make(chan struct{})}
	runChief(&opts, cfg, turns)
	s, c := start(t, opts)

	first, err := c.Send(ctx, stream, "Where is the plan?")
	must(t, err)
	if first.Kind != "message" || first.State != TurnQueued || first.Text != "Where is the plan?" || !strings.HasPrefix(first.Turn, "message_") || first.At.IsZero() {
		t.Fatalf("acknowledgement: %+v", first)
	}
	// The acknowledged message is already in the trace.
	th, err := s.active.repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != first.Turn {
		t.Fatalf("thread after send: %+v", th)
	}
	select {
	case <-turns.entered:
	case <-time.After(demoTimeout):
		t.Fatal("message did not run as a turn")
	}
	second, err := c.Send(ctx, stream, "fail")
	must(t, err)
	cancelled, err := c.Send(ctx, stream, "cancel")
	must(t, err)
	silent, err := c.Send(ctx, stream, "silent")
	must(t, err)
	third, err := c.Send(ctx, stream, "Ship it")
	must(t, err)
	list, err := c.Conversation(ctx, stream)
	must(t, err)
	if got := states(list); !reflect.DeepEqual(got, []string{"message:running", "message:queued", "message:queued", "message:queued", "message:queued"}) {
		t.Fatalf("mid-turn listing: %v", got)
	}
	close(turns.hold)

	list = awaitConversation(t, c, func(l ConversationResponse) bool { return len(l.Entries) == 9 })
	want := []ConversationEntry{
		{Turn: first.Turn, Kind: "message", Text: "Where is the plan?", At: first.At, State: TurnDone},
		{Turn: first.Turn, Kind: "response", Text: "Answer to Where is the plan?", State: TurnDone},
		{Turn: second.Turn, Kind: "message", Text: "fail", At: second.At, State: TurnFailed},
		{Turn: second.Turn, Kind: "response", Text: "partial", State: TurnFailed},
		{Turn: cancelled.Turn, Kind: "message", Text: "cancel", At: cancelled.At, State: TurnFailed},
		{Turn: cancelled.Turn, Kind: "response", Text: "stopped", State: TurnFailed},
		{Turn: silent.Turn, Kind: "message", Text: "silent", At: silent.At, State: TurnDone},
		{Turn: third.Turn, Kind: "message", Text: "Ship it", At: third.At, State: TurnDone},
		{Turn: third.Turn, Kind: "response", Text: "Answer to Ship it", State: TurnDone},
	}
	for i, e := range list.Entries {
		if e.Kind == "response" {
			if !e.At.After(list.Entries[i-1].At) {
				t.Fatalf("response time %s is not after its message", e.At)
			}
			want[i].At = e.At
		}
	}
	if list.Workstream != stream || !reflect.DeepEqual(list.Entries, want) {
		t.Fatalf("listing:\n%+v\nwant:\n%+v", list.Entries, want)
	}

	calls := turns.Calls()
	if len(calls) != 5 {
		t.Fatalf("turns run: %d", len(calls))
	}
	for i, prompt := range []string{"Where is the plan?", "fail", "cancel", "silent", "Ship it"} {
		p := calls[i]
		if p.Prompt != prompt || p.Scope.Role != trace.ChiefOfStaff || p.Scope.Thread != trace.ChiefOfStaff || p.Profile.Name != "default" {
			t.Fatalf("turn %d: %+v", i, p)
		}
		for _, part := range []string{"You are the chief of staff for workstream " + string(stream), "# Project context", "workstream: " + string(stream), "charter#1 [Rules]: Keep the upload API stable."} {
			if !strings.Contains(p.SystemPrompt, part) {
				t.Fatalf("turn %d system prompt lacks %q:\n%s", i, part, p.SystemPrompt)
			}
		}
	}

	// Turns the owner did not send are not part of the conversation.
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_question", Project: project, Workstream: stream, At: demoStart, Actor: trace.Actor{Kind: "agent", ID: "mason"}, Cause: "ask"}
	_, err = s.active.repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "question", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, Prompt: "A mason asks"})
	must(t, err)
	if again, err := c.Conversation(ctx, stream); err != nil || len(again.Entries) != 9 {
		t.Fatalf("listing with an agent turn: %+v %v", again, err)
	}

	empty, err := c.Conversation(ctx, quiet)
	must(t, err)
	if empty.Workstream != quiet || empty.Entries == nil || len(empty.Entries) != 0 {
		t.Fatalf("quiet workstream: %+v", empty)
	}
}

func TestConversationRejections(t *testing.T) {
	ctx := context.Background()
	opts, _ := conversationFixture(t, "cr-")
	s, c := start(t, opts)
	unknown := config.WorkstreamID("w_00000000000000000000000000000009")
	expect := func(err error, code Code, message string) {
		t.Helper()
		var api *APIError
		if !errors.As(err, &api) || api.Code != code || !strings.Contains(api.Message, message) {
			t.Fatalf("got %v, want %s %q", err, code, message)
		}
	}
	_, err := c.Send(ctx, unknown, "hello")
	expect(err, Validation, "workstream "+string(unknown)+" is not in the active project")
	_, err = c.Conversation(ctx, unknown)
	expect(err, Validation, "workstream "+string(unknown)+" is not in the active project")
	_, err = c.Send(ctx, "w_bad", "hello")
	expect(err, Validation, "workstream must be a workstream ID")
	_, err = c.Conversation(ctx, "w_bad")
	expect(err, Validation, "workstream must be a workstream ID")
	_, err = c.Send(ctx, stream, " \n")
	expect(err, Validation, "text must not be empty")
	err = c.Do(ctx, "POST", Prefix+"/conversation/"+string(stream), map[string]string{"message": "hello"}, nil)
	expect(err, Malformed, "")
	err = c.Do(ctx, "PUT", Prefix+"/conversation/"+string(stream), SendRequest{Text: "hello"}, nil)
	expect(err, Unsupported, "")
	th, err := s.active.repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 0 {
		t.Fatalf("rejected messages were recorded: %+v", th.Turns)
	}

	// Without a trace the workstream is unknown.
	unbound := fixture(t)
	_, c = start(t, unbound)
	_, err = c.Send(ctx, stream, "hello")
	expect(err, Validation, "workstream "+string(stream)+" is not in the active project")

	empty, _ := projectFixture(t)
	_, c = start(t, empty)
	_, err = c.Send(ctx, stream, "hello")
	expect(err, NoProject, "no project is configured")
	_, err = c.Conversation(ctx, stream)
	expect(err, NoProject, "no project is configured")
}

func TestConversationUsesProfileOverride(t *testing.T) {
	ctx := context.Background()
	opts, _ := conversationFixture(t, "co-")
	file := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(file)
	must(t, err)
	must(t, os.WriteFile(file, []byte(strings.Replace(string(data), "model = \"test\"\n", "model = \"test\"\nfallback = \"other\"\n", 1)), 0600))
	s, c := start(t, opts)
	mutation(t, c, "PUT", "profile", ProfileRequest{trace.ChiefOfStaff, "other"})
	sent, err := c.Send(ctx, stream, "hello")
	must(t, err)
	th, err := s.active.repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != sent.Turn || th.Turns[0].Request.Profile.Name != "other" || th.Turns[0].Request.Profile.Backend != "codex" {
		t.Fatalf("accepted profile: %+v", th.Turns)
	}
}

func TestConversationUsesOverrideOutsideFallbackChain(t *testing.T) {
	opts, _ := conversationFixture(t, "cf-")
	s, c := start(t, opts)
	mutation(t, c, "PUT", "profile", ProfileRequest{trace.ChiefOfStaff, "other"})
	sent, err := c.Send(context.Background(), stream, "hello")
	must(t, err)
	th, err := s.active.repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != sent.Turn || th.Turns[0].Request.Profile.Name != "other" || th.Turns[0].Request.Profile.Backend != "codex" {
		t.Fatalf("accepted profile: %+v", th.Turns)
	}
}

func TestConversationMessageRunsOnceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "cx-")
	// First lifetime: no turn runner, so the message is acknowledged and the
	// service stops before it runs.
	s, c := start(t, opts)
	sent, err := c.Send(ctx, stream, "Plan the upload work")
	must(t, err)
	c.Close()
	must(t, s.Close())

	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	th, err := repo.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != sent.Turn || th.Turns[0].Claim != nil {
		t.Fatalf("durable message: %+v", th.Turns)
	}
	must(t, repo.Close())

	for lifetime := 2; lifetime <= 3; lifetime++ {
		turns := &chiefTurns{}
		runChief(&opts, cfg, turns)
		s, c = start(t, opts)
		list := awaitConversation(t, c, func(l ConversationResponse) bool { return len(l.Entries) == 2 })
		if got := states(list); !reflect.DeepEqual(got, []string{"message:done", "response:done"}) || list.Entries[1].Text != "Answer to Plan the upload work" {
			t.Fatalf("lifetime %d listing: %+v", lifetime, list)
		}
		c.Close()
		must(t, s.Close())
		want := 0
		if lifetime == 2 {
			want = 1
		}
		if calls := turns.Calls(); len(calls) != want {
			t.Fatalf("lifetime %d ran %d turns", lifetime, len(calls))
		}
	}
	repo, err = trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	defer repo.Close()
	ops, err := repo.Operations(stream)
	must(t, err)
	if len(ops) != 1 {
		t.Fatalf("turn operations: %+v", ops)
	}
}

func TestConversationUnusableProfileIsInternal(t *testing.T) {
	opts, _ := conversationFixture(t, "cu-")
	s, c := start(t, opts)
	s.mu.Lock()
	cfg := *s.cfg
	cfg.Profiles = map[string]config.Profile{}
	s.cfg = &cfg
	s.mu.Unlock()
	_, err := c.Send(context.Background(), stream, "hello")
	var api *APIError
	if !errors.As(err, &api) || api.Code != Internal || !strings.Contains(api.Message, "role chief_of_staff has no usable profile \"default\"") {
		t.Fatalf("unusable profile: %v", err)
	}
	th, err := s.active.repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 0 {
		t.Fatalf("message recorded without a profile: %+v", th.Turns)
	}
}

func TestConversationListsInterruptedTurnAsFailed(t *testing.T) {
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "ci-")
	s, c := start(t, opts)
	sent, err := c.Send(ctx, stream, "Plan the upload work")
	must(t, err)
	queued, err := c.Send(ctx, stream, "And the docs")
	must(t, err)
	c.Close()
	must(t, s.Close())

	// An earlier service session claimed the turn and stopped before
	// capturing a result.
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token", filepath.Join(cfg.Root.String(), "sessions", "lost"), demoStart.Add(time.Hour))
	must(t, err)
	must(t, repo.Close())

	_, c = start(t, opts)
	list, err := c.Conversation(ctx, stream)
	must(t, err)
	want := []ConversationEntry{
		{Turn: sent.Turn, Kind: "message", Text: "Plan the upload work", At: sent.At, State: TurnFailed},
		{Turn: queued.Turn, Kind: "message", Text: "And the docs", At: queued.At, State: TurnQueued},
	}
	if !reflect.DeepEqual(list.Entries, want) {
		t.Fatalf("listing after restart:\n%+v\nwant:\n%+v", list.Entries, want)
	}
}
