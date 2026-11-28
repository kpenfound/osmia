package executionjourney

import (
	context "context"
	errors "errors"
	os "os"
	filepath "path/filepath"
	reflect "reflect"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestConversationRunsMessagesInOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "cv-")
	turns := &fixturekit.ManagerTurns{Hold: make(chan struct{}), Entered: make(chan struct{})}
	journey.RunManager(&opts, cfg, turns)
	s, c := journey.StartT(t, opts)

	first, err := c.Send(ctx, fixturekit.Stream, "Where is the plan?")
	fixturekit.Must(t, err)
	if first.Kind != "message" || first.State != api.TurnQueued || first.Text != "Where is the plan?" || !strings.HasPrefix(first.Turn, "message_") || first.At.IsZero() {
		t.Fatalf("acknowledgement: %+v", first)
	}

	th, err := s.Sole().Repository.ManagerThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != first.Turn {
		t.Fatalf("thread after send: %+v", th)
	}
	select {
	case <-turns.Entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("message did not run as a turn")
	}
	second, err := c.Send(ctx, fixturekit.Stream, "fail")
	fixturekit.Must(t, err)
	cancelled, err := c.Send(ctx, fixturekit.Stream, "cancel")
	fixturekit.Must(t, err)
	silent, err := c.Send(ctx, fixturekit.Stream, "silent")
	fixturekit.Must(t, err)
	third, err := c.Send(ctx, fixturekit.Stream, "Ship it")
	fixturekit.Must(t, err)
	list, err := c.Conversation(ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	if got := intakefixture.States(list); !reflect.DeepEqual(got, []string{"message:running", "message:queued", "message:queued", "message:queued", "message:queued"}) {
		t.Fatalf("mid-turn listing: %v", got)
	}
	close(turns.Hold)

	list = intakefixture.AwaitConversation(t, c, func(l api.ConversationResponse) bool { return len(l.Entries) == 9 })
	want := []api.ConversationEntry{
		{Turn: first.Turn, Kind: "message", Text: "Where is the plan?", At: first.At, State: api.TurnDone},
		{Turn: first.Turn, Kind: "response", Text: "Answer to Where is the plan?", State: api.TurnDone},
		{Turn: second.Turn, Kind: "message", Text: "fail", At: second.At, State: api.TurnFailed},
		{Turn: second.Turn, Kind: "response", Text: "partial", State: api.TurnFailed},
		{Turn: cancelled.Turn, Kind: "message", Text: "cancel", At: cancelled.At, State: api.TurnFailed},
		{Turn: cancelled.Turn, Kind: "response", Text: "stopped", State: api.TurnFailed},
		{Turn: silent.Turn, Kind: "message", Text: "silent", At: silent.At, State: api.TurnDone},
		{Turn: third.Turn, Kind: "message", Text: "Ship it", At: third.At, State: api.TurnDone},
		{Turn: third.Turn, Kind: "response", Text: "Answer to Ship it", State: api.TurnDone},
	}
	for i, e := range list.Entries {
		if e.Kind == "response" {
			if !e.At.After(list.Entries[i-1].At) {
				t.Fatalf("response time %s is not after its message", e.At)
			}
			want[i].At = e.At
		}
	}
	if list.Workstream != fixturekit.Stream || !reflect.DeepEqual(list.Entries, want) {
		t.Fatalf("listing:\n%+v\nwant:\n%+v", list.Entries, want)
	}

	calls := turns.Calls()
	if len(calls) != 5 {
		t.Fatalf("turns run: %d", len(calls))
	}
	for i, prompt := range []string{"Where is the plan?", "fail", "cancel", "silent", "Ship it"} {
		p := calls[i]
		if p.Prompt != prompt || p.Scope.Role != trace.Manager || p.Scope.Thread != trace.Manager || p.Profile.Name != "default" {
			t.Fatalf("turn %d: %+v", i, p)
		}
		if !strings.Contains(p.SystemPrompt, "You are the chief of staff for workstream "+string(fixturekit.Stream)) || strings.Contains(p.SystemPrompt, "# Project context") {
			t.Fatalf("turn %d system prompt %q", i, p.SystemPrompt)
		}
		current := strings.Join(p.Context, "\n")
		for _, part := range []string{"# Project context", "workstream: " + string(fixturekit.Stream), "charter#1 [Rules]: Keep the upload API stable.", "## Latest status", "## Open inbox escalations"} {
			if !strings.Contains(current, part) {
				t.Fatalf("turn %d context lacks %q:\n%s", i, part, current)
			}
		}
	}

	h := trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request_question", Project: fixturekit.Project, Workstream: fixturekit.Stream, At: fixturekit.DemoStart, Actor: trace.Actor{Kind: "agent", ID: "mason"}, Cause: "ask"}
	_, err = s.Sole().Repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.Manager, ThreadID: trace.Manager, TurnID: "question", Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, Prompt: "A mason asks"})
	fixturekit.Must(t, err)
	if again, err := c.Conversation(ctx, fixturekit.Stream); err != nil || len(again.Entries) != 9 {
		t.Fatalf("listing with an agent turn: %+v %v", again, err)
	}

	empty, err := c.Conversation(ctx, fixturekit.Quiet)
	fixturekit.Must(t, err)
	if empty.Workstream != fixturekit.Quiet || empty.Entries == nil || len(empty.Entries) != 0 {
		t.Fatalf("quiet workstream: %+v", empty)
	}
}

func TestConversationRejections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, _ := journey.ConversationFixture(t, "cr-")
	s, c := journey.StartT(t, opts)
	unknown := config.WorkstreamID("w_00000000000000000000000000000009")
	expect := func(err error, code api.Code, message string) {
		t.Helper()
		var apiErr *api.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != code || !strings.Contains(apiErr.Message, message) {
			t.Fatalf("got %v, want %s %q", err, code, message)
		}
	}
	_, err := c.Send(ctx, unknown, "hello")
	expect(err, api.Validation, "workstream "+string(unknown)+" is not in an active project")
	_, err = c.Conversation(ctx, unknown)
	expect(err, api.Validation, "workstream "+string(unknown)+" is not in an active project")
	_, err = c.Send(ctx, "w_bad", "hello")
	expect(err, api.Validation, "workstream must be a workstream ID")
	_, err = c.Conversation(ctx, "w_bad")
	expect(err, api.Validation, "workstream must be a workstream ID")
	_, err = c.Send(ctx, fixturekit.Stream, " \n")
	expect(err, api.Validation, "text must not be empty")
	err = c.Do(ctx, "POST", service.Prefix+"/conversation/"+string(fixturekit.Stream), map[string]string{"message": "hello"}, nil)
	expect(err, api.Malformed, "")
	err = c.Do(ctx, "PUT", service.Prefix+"/conversation/"+string(fixturekit.Stream), api.SendRequest{Text: "hello"}, nil)
	expect(err, api.Unsupported, "")
	th, err := s.Sole().Repository.ManagerThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(th.Turns) != 0 {
		t.Fatalf("rejected messages were recorded: %+v", th.Turns)
	}

	unbound := journey.Fixture(t)
	_, c = journey.StartT(t, unbound)
	_, err = c.Send(ctx, fixturekit.Stream, "hello")
	expect(err, api.Validation, "workstream "+string(fixturekit.Stream)+" is not in an active project")

	empty, _ := journey.ProjectFixture(t)
	_, c = journey.StartT(t, empty)
	_, err = c.Send(ctx, fixturekit.Stream, "hello")
	expect(err, api.NoProject, "no project is configured")
	_, err = c.Conversation(ctx, fixturekit.Stream)
	expect(err, api.NoProject, "no project is configured")
}

func TestConversationProfileChangesResumeOrReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "cp-")
	file := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(file)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(file, append(data, []byte("\n[profiles.same]\nagent = \"claude\"\nmodel = \"same\"\n")...), 0600))
	turns := &fixturekit.ManagerTurns{Hold: make(chan struct{}), Entered: make(chan struct{})}
	journey.RunManager(&opts, cfg, turns)
	s, c := journey.StartT(t, opts)
	_, err = c.Send(ctx, fixturekit.Stream, "first")
	fixturekit.Must(t, err)
	select {
	case <-turns.Entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("first turn did not start")
	}
	intakefixture.Mutation(t, c, "PUT", "profile", api.ProfileRequest{Role: trace.Manager, Profile: "same"})
	if calls := turns.Calls(); len(calls) != 1 || calls[0].Profile.Name != "default" {
		t.Fatalf("in-flight profile changed: %+v", calls)
	}
	close(turns.Hold)
	intakefixture.AwaitConversation(t, c, func(l api.ConversationResponse) bool { return len(l.Entries) == 2 })
	for i, step := range []struct{ profile, prompt string }{{"same", "second"}, {"other", "third"}, {"", "fourth"}} {
		if i == 2 {
			intakefixture.Mutation(t, c, "DELETE", "profile", api.ClearProfileRequest{Role: trace.Manager})
		} else if i > 0 {
			intakefixture.Mutation(t, c, "PUT", "profile", api.ProfileRequest{Role: trace.Manager, Profile: step.profile})
		}
		_, err := c.Send(ctx, fixturekit.Stream, step.prompt)
		fixturekit.Must(t, err)
		intakefixture.AwaitConversation(t, c, func(l api.ConversationResponse) bool { return len(l.Entries) == (i+2)*2 })
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
	th, err := s.Sole().Repository.ManagerThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(th.Turns) != 4 || th.Turns[0].Request.Profile.Name != "default" || th.Turns[3].Request.Profile.Name != "default" {
		t.Fatalf("recorded profiles: %+v", th.Turns)
	}
}

func TestConversationUsesOverrideOutsideFallbackChain(t *testing.T) {
	t.Parallel()
	opts, _ := journey.ConversationFixture(t, "cf-")
	s, c := journey.StartT(t, opts)
	intakefixture.Mutation(t, c, "PUT", "profile", api.ProfileRequest{Role: trace.Manager, Profile: "other"})
	sent, err := c.Send(context.Background(), fixturekit.Stream, "hello")
	fixturekit.Must(t, err)
	th, err := s.Sole().Repository.ManagerThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != sent.Turn || th.Turns[0].Request.Profile.Name != "other" || th.Turns[0].Request.Profile.Backend != "codex" {
		t.Fatalf("accepted profile: %+v", th.Turns)
	}
}

func TestConversationMessageRunsOnceAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "cx-")

	s, c := journey.StartT(t, opts)
	sent, err := c.Send(ctx, fixturekit.Stream, "Plan the upload work")
	fixturekit.Must(t, err)
	c.Close()
	fixturekit.Must(t, s.Close())

	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	th, err := repo.ManagerThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.TurnID != sent.Turn || th.Turns[0].Claim != nil {
		t.Fatalf("durable message: %+v", th.Turns)
	}
	fixturekit.Must(t, repo.Close())

	for lifetime := 2; lifetime <= 3; lifetime++ {
		turns := &fixturekit.ManagerTurns{}
		journey.RunManager(&opts, cfg, turns)
		s, c = journey.StartT(t, opts)
		list := intakefixture.AwaitConversation(t, c, func(l api.ConversationResponse) bool { return len(l.Entries) == 2 })
		if got := intakefixture.States(list); !reflect.DeepEqual(got, []string{"message:done", "response:done"}) || list.Entries[1].Text != "Answer to Plan the upload work" {
			t.Fatalf("lifetime %d listing: %+v", lifetime, list)
		}
		c.Close()
		fixturekit.Must(t, s.Close())
		want := 0
		if lifetime == 2 {
			want = 1
		}
		if calls := turns.Calls(); len(calls) != want {
			t.Fatalf("lifetime %d ran %d turns", lifetime, len(calls))
		}
	}
	repo, err = trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	defer repo.Close()
	ops, err := repo.Operations(fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(ops) != 1 {
		t.Fatalf("turn operations: %+v", ops)
	}
}

func TestConversationUnusableProfileIsInternal(t *testing.T) {
	t.Parallel()
	opts, _ := journey.ConversationFixture(t, "cu-")
	s, c := journey.StartT(t, opts)
	cfg := *s.Current()
	cfg.Profiles = map[string]config.Profile{}
	service.InspectForTest(s.Core()).SetConfig(&cfg)
	s.Cfg = &cfg
	_, err := c.Send(context.Background(), fixturekit.Stream, "hello")
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != api.Internal || !strings.Contains(apiErr.Message, "role manager has no usable profile \"default\"") {
		t.Fatalf("unusable profile: %v", err)
	}
	th, err := s.Sole().Repository.ManagerThread(fixturekit.Stream)
	fixturekit.Must(t, err)
	if len(th.Turns) != 0 {
		t.Fatalf("message recorded without a profile: %+v", th.Turns)
	}
}

func TestConversationListsInterruptedTurnAsFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := journey.ConversationFixture(t, "ci-")
	s, c := journey.StartT(t, opts)
	sent, err := c.Send(ctx, fixturekit.Stream, "Plan the upload work")
	fixturekit.Must(t, err)
	queued, err := c.Send(ctx, fixturekit.Stream, "And the docs")
	fixturekit.Must(t, err)
	c.Close()
	fixturekit.Must(t, s.Close())

	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, fixturekit.Stream, trace.Manager, "token", filepath.Join(cfg.Root.String(), "sessions", "lost"), fixturekit.DemoStart.Add(time.Hour))
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Close())

	_, c = journey.StartT(t, opts)
	list, err := c.Conversation(ctx, fixturekit.Stream)
	fixturekit.Must(t, err)
	want := []api.ConversationEntry{
		{Turn: sent.Turn, Kind: "message", Text: "Plan the upload work", At: sent.At, State: api.TurnFailed},
		{Turn: queued.Turn, Kind: "message", Text: "And the docs", At: queued.At, State: api.TurnQueued, QueuedReason: "starting"},
	}
	if !reflect.DeepEqual(list.Entries, want) {
		t.Fatalf("listing after restart:\n%+v\nwant:\n%+v", list.Entries, want)
	}
}
