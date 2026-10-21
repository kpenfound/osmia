package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// conversationFixture creates a project with a trace holding two workstreams
// and a charter rule.
func conversationFixture(t *testing.T, prefix string) (Options, *config.Config) {
	t.Helper()
	opts, cfg := fixturekit.Conversation(t, prefix)
	o := testOptions(opts)
	o.Reconciliation.Now = (&demoClock{At: demoStart}).Now
	return o, cfg
}

// runChief binds the fake chief of staff through the thread dispatcher. It
// exercises the Runner's resume mechanism directly, as chiefTurns always
// authorizes resume; production confines that capability to the session role
// alone (thread.Runner.ResumableRoles, set only in internal/service/enforcement.go).
func runChief(opts *Options, cfg *config.Config, turns *chiefTurns) {
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: opts.Reconciliation.Now, ResumableRoles: func(string) bool { return true }},
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
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "cv-")
	turns := &chiefTurns{Hold: make(chan struct{}), Entered: make(chan struct{})}
	runChief(&opts, cfg, turns)
	s, c := start(t, opts)

	first, err := c.Send(ctx, stream, "Where is the plan?")
	must(t, err)
	if first.Kind != "message" || first.State != TurnQueued || first.Text != "Where is the plan?" || !strings.HasPrefix(first.Turn, "message_") || first.At.IsZero() {
		t.Fatalf("acknowledgement: %+v", first)
	}
	// The acknowledged message is already in the trace.
	th, err := s.sole().repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != first.Turn {
		t.Fatalf("thread after send: %+v", th)
	}
	select {
	case <-turns.Entered:
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
	close(turns.Hold)

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
		for _, part := range []string{"You are the chief of staff for workstream " + string(stream), "# Project context", "workstream: " + string(stream), "charter#1 [Rules]: Keep the upload API stable.", "## Latest status", "## Open inbox escalations"} {
			if !strings.Contains(p.SystemPrompt, part) {
				t.Fatalf("turn %d system prompt lacks %q:\n%s", i, part, p.SystemPrompt)
			}
		}
	}

	// Turns the owner did not send are not part of the conversation.
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_question", Project: project, Workstream: stream, At: demoStart, Actor: trace.Actor{Kind: "agent", ID: "mason"}, Cause: "ask"}
	_, err = s.sole().repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "question", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, Prompt: "A mason asks"})
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
	t.Parallel()
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
	expect(err, Validation, "workstream "+string(unknown)+" is not in an active project")
	_, err = c.Conversation(ctx, unknown)
	expect(err, Validation, "workstream "+string(unknown)+" is not in an active project")
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
	th, err := s.sole().repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 0 {
		t.Fatalf("rejected messages were recorded: %+v", th.Turns)
	}

	// The librarian's workstream is in the trace but not in status, and its
	// chief of staff never gets a turn: it is unknown to the conversation too.
	librarian, err := ensureLibrarianThread(ctx, s.sole().repository, demoStart, ownerActor)
	must(t, err)
	_, err = c.Send(ctx, librarian, "hello")
	expect(err, Validation, "workstream "+string(librarian)+" is not in an active project")
	_, err = c.Conversation(ctx, librarian)
	expect(err, Validation, "workstream "+string(librarian)+" is not in an active project")
	th, err = s.sole().repository.ChiefOfStaffThread(librarian)
	must(t, err)
	if len(th.Turns) != 0 {
		t.Fatalf("a message to the librarian's workstream was recorded: %+v", th.Turns)
	}

	// Without a trace the workstream is unknown.
	unbound := fixture(t)
	_, c = start(t, unbound)
	_, err = c.Send(ctx, stream, "hello")
	expect(err, Validation, "workstream "+string(stream)+" is not in an active project")

	empty, _ := projectFixture(t)
	_, c = start(t, empty)
	_, err = c.Send(ctx, stream, "hello")
	expect(err, NoProject, "no project is configured")
	_, err = c.Conversation(ctx, stream)
	expect(err, NoProject, "no project is configured")
}

func TestConversationProfileChangesResumeOrReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "cp-")
	file := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(file)
	must(t, err)
	must(t, os.WriteFile(file, append(data, []byte("\n[profiles.same]\nagent = \"claude\"\nmodel = \"same\"\n")...), 0600))
	turns := &chiefTurns{Hold: make(chan struct{}), Entered: make(chan struct{})}
	runChief(&opts, cfg, turns)
	s, c := start(t, opts)
	_, err = c.Send(ctx, stream, "first")
	must(t, err)
	select {
	case <-turns.Entered:
	case <-time.After(demoTimeout):
		t.Fatal("first turn did not start")
	}
	mutation(t, c, "PUT", "profile", ProfileRequest{Role: trace.ChiefOfStaff, Profile: "same"})
	if calls := turns.Calls(); len(calls) != 1 || calls[0].Profile.Name != "default" {
		t.Fatalf("in-flight profile changed: %+v", calls)
	}
	close(turns.Hold)
	awaitConversation(t, c, func(l ConversationResponse) bool { return len(l.Entries) == 2 })
	for i, step := range []struct{ profile, prompt string }{{"same", "second"}, {"other", "third"}, {"", "fourth"}} {
		if i == 2 {
			mutation(t, c, "DELETE", "profile", ClearProfileRequest{Role: trace.ChiefOfStaff})
		} else if i > 0 {
			mutation(t, c, "PUT", "profile", ProfileRequest{Role: trace.ChiefOfStaff, Profile: step.profile})
		}
		_, err := c.Send(ctx, stream, step.prompt)
		must(t, err)
		awaitConversation(t, c, func(l ConversationResponse) bool { return len(l.Entries) == (i+2)*2 })
	}
	calls := turns.Calls()
	if len(calls) != 4 {
		t.Fatalf("turn calls: %d", len(calls))
	}
	for i, want := range []struct{ name, path string }{{"default", "replay"}, {"same", "resume"}, {"other", "replay"}, {"default", "replay"}} {
		path := "replay"
		if calls[i].Resume != nil {
			path = "resume"
		}
		if calls[i].Profile.Name != want.name || path != want.path {
			t.Fatalf("turn %d: profile %q path %s", i, calls[i].Profile.Name, path)
		}
		if want.path == "replay" && i > 0 && !strings.Contains(calls[i].History, "Answer to first") {
			t.Fatalf("turn %d omitted owned log: %q", i, calls[i].History)
		}
	}
	th, err := s.sole().repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 4 || th.Turns[0].Request.Profile.Name != "default" || th.Turns[3].Request.Profile.Name != "default" {
		t.Fatalf("recorded profiles: %+v", th.Turns)
	}
}

func TestConversationUsesOverrideOutsideFallbackChain(t *testing.T) {
	t.Parallel()
	opts, _ := conversationFixture(t, "cf-")
	s, c := start(t, opts)
	mutation(t, c, "PUT", "profile", ProfileRequest{Role: trace.ChiefOfStaff, Profile: "other"})
	sent, err := c.Send(context.Background(), stream, "hello")
	must(t, err)
	th, err := s.sole().repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != sent.Turn || th.Turns[0].Request.Profile.Name != "other" || th.Turns[0].Request.Profile.Backend != "codex" {
		t.Fatalf("accepted profile: %+v", th.Turns)
	}
}

func TestConversationMessageRunsOnceAcrossRestart(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	th, err := s.sole().repository.ChiefOfStaffThread(stream)
	must(t, err)
	if len(th.Turns) != 0 {
		t.Fatalf("message recorded without a profile: %+v", th.Turns)
	}
}

func TestConversationListsInterruptedTurnAsFailed(t *testing.T) {
	t.Parallel()
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
		{Turn: queued.Turn, Kind: "message", Text: "And the docs", At: queued.At, State: TurnQueued, QueuedReason: "starting"},
	}
	if !reflect.DeepEqual(list.Entries, want) {
		t.Fatalf("listing after restart:\n%+v\nwant:\n%+v", list.Entries, want)
	}
}
