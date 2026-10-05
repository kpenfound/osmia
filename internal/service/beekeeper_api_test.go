package service

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service/beekeeper"
	"github.com/kpenfound/osmia/internal/trace"
)

// beekeeperAPIError unwraps the *APIError a Client call against a failed
// request returns, or fails the test.
func beekeeperAPIError(t *testing.T, err error) *APIError {
	t.Helper()
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("expected an API error, got %v", err)
	}
	return api
}

// seedBeekeeperTurn records and completes one owner request and Beekeeper
// reply directly on the shadow repository's generic turn functions, the same
// way beekeeper_turns_test.go's restart test builds a Beekeeper turn by
// hand, so a test can assemble the Beekeeper chat's history without a model
// session.
func seedBeekeeperTurn(t *testing.T, repo *trace.Repository, id string, at time.Time, prompt, reply string) {
	t.Helper()
	ctx := context.Background()
	req := trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + id, Revision: 1,
			Project: config.ShadowProjectID, Workstream: config.BeekeeperWorkstreamID, At: at, Actor: beekeeper.OwnerActor, Cause: "owner-message"},
		AgentID: beekeeper.AgentID, ThreadID: beekeeper.ThreadID, TurnID: "message_" + id,
		Profile: coreadapter.Profile{Name: "default", Backend: "claude", Model: "test"}, Prompt: prompt,
	}
	if _, err := repo.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	q, err := repo.ClaimTurn(ctx, config.BeekeeperWorkstreamID, beekeeper.AgentID, "token-"+id, "/session/"+id, at)
	must(t, err)
	respAt := at.Add(time.Millisecond)
	h := req.Header
	h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", "response_"+id, respAt, beekeeper.ReplyActor
	resp := trace.TurnResponse{
		Header: h, AgentID: beekeeper.AgentID, ThreadID: beekeeper.ThreadID, TurnID: req.TurnID, RequestID: req.ID, RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "session-" + id}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: at, Duration: time.Millisecond, FinalResponse: reply},
	}
	must(t, repo.CaptureTurn(ctx, q.Claim.Token, resp))
	must(t, repo.CompleteTurn(ctx, config.BeekeeperWorkstreamID, beekeeper.AgentID, req.TurnID, q.Claim.Token, respAt))
}

// Parsing the limit query parameter of GET /v1/beekeeper: absent uses the
// documented default of 50, an explicit value is honoured, and a value
// above the documented cap of 200 is clamped to it.
func TestBeekeeperLimitParsing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		want  int
		fail  bool
	}{
		{"", defaultBeekeeperMessages, false},
		{"limit=5", 5, false},
		{"limit=200", 200, false},
		{"limit=100000", maxBeekeeperMessages, false},
		{"limit=0", 0, true},
		{"limit=-3", 0, true},
		{"limit=abc", 0, true},
	}
	if defaultBeekeeperMessages != 50 || maxBeekeeperMessages != 200 {
		t.Fatalf("documented default and cap: %d %d", defaultBeekeeperMessages, maxBeekeeperMessages)
	}
	for _, c := range cases {
		url := Prefix + "/beekeeper"
		if c.query != "" {
			url += "?" + c.query
		}
		r := httptest.NewRequest("GET", url, nil)
		n, api := beekeeperLimit(r)
		if c.fail {
			if api == nil || api.Code != Validation {
				t.Errorf("query %q: expected a validation error, got %d %v", c.query, n, api)
			}
			continue
		}
		if api != nil || n != c.want {
			t.Errorf("query %q: got %d %v, want %d", c.query, n, api, c.want)
		}
	}
}

// The Beekeeper chat's messages come back oldest first with their authors,
// including a relayed chief-of-staff reply, through the HTTP layer alone;
// the default limit of 50 is applied, an explicit limit is honoured, a limit
// above the server's cap is clamped to it, and has-older is correct in every
// case. The endpoint path names no project.
func TestBeekeeperMessagesHandler(t *testing.T) {
	t.Parallel()
	for _, route := range Routes {
		if strings.HasPrefix(route.Path, Prefix+"/beekeeper") && strings.Contains(route.Path, "{") {
			t.Fatalf("beekeeper route names a path parameter: %s", route.Path)
		}
	}
	opts := fixture(t)
	clock := &fixedClock{now: demoStart}
	opts.Reconciliation.Now = clock.Now
	s, c := start(t, opts)
	ctx := context.Background()
	repo := s.Beekeeper()

	// A full second between ticks keeps every seeded timestamp strictly
	// increasing and clear of seedBeekeeperTurn's own 1ms request-to-reply
	// offset, so ordering never depends on how ties between equal
	// timestamps happen to be broken.
	tick := func() time.Time { v := clock.now; clock.Advance(time.Second); return v }
	seedBeekeeperTurn(t, repo, "1", tick(), "Status please.", "All quiet.")
	must(t, beekeeper.RecordRelayedReply(ctx, repo, project, stream, "relay-1", "Shipped the draft.", tick()))
	seedBeekeeperTurn(t, repo, "2", tick(), "Anything else?", "Nothing else.")

	all, err := c.Beekeeper(ctx, 0)
	must(t, err)
	wantKinds := []string{beekeeper.AuthorOwner, beekeeper.AuthorBeekeeper, beekeeper.AuthorChiefOfStaff, beekeeper.AuthorOwner, beekeeper.AuthorBeekeeper}
	if len(all.Messages) != len(wantKinds) || all.HasOlder {
		t.Fatalf("default window with %d entries: %+v", len(wantKinds), all)
	}
	for i, m := range all.Messages {
		if m.Author.Kind != wantKinds[i] {
			t.Fatalf("message %d author: %+v, want %s", i, m, wantKinds[i])
		}
		if i > 0 && m.At.Before(all.Messages[i-1].At) {
			t.Fatal("messages not oldest first")
		}
	}
	if all.Messages[2].Author.Workstream != stream || all.Messages[2].Text != "Shipped the draft." {
		t.Fatalf("relayed reply entry: %+v", all.Messages[2])
	}

	limited, err := c.Beekeeper(ctx, 2)
	must(t, err)
	if len(limited.Messages) != 2 || !limited.HasOlder {
		t.Fatalf("explicit limit of 2: %+v", limited)
	}
	if limited.Messages[0] != all.Messages[3] || limited.Messages[1] != all.Messages[4] {
		t.Fatalf("explicit limit did not return the most recent 2 messages: %+v", limited)
	}

	// Push the total well past the server's cap with cheap relayed replies,
	// so a limit above it is genuinely clamped rather than merely returning
	// everything there is.
	const bulk = 210
	for i := 0; i < bulk; i++ {
		must(t, beekeeper.RecordRelayedReply(ctx, repo, project, stream, fmt.Sprintf("bulk-%d", i), fmt.Sprintf("bulk reply %d", i), tick()))
	}
	total := len(wantKinds) + bulk

	byDefault, err := c.Beekeeper(ctx, 0)
	must(t, err)
	if len(byDefault.Messages) != defaultBeekeeperMessages || !byDefault.HasOlder {
		t.Fatalf("default limit with %d total messages: got %d, hasOlder %v", total, len(byDefault.Messages), byDefault.HasOlder)
	}
	if byDefault.Messages[0].Text != fmt.Sprintf("bulk reply %d", bulk-defaultBeekeeperMessages) {
		t.Fatalf("default window is not the most recent %d: %+v", defaultBeekeeperMessages, byDefault.Messages[0])
	}

	clamped, err := c.Beekeeper(ctx, 100000)
	must(t, err)
	if len(clamped.Messages) != maxBeekeeperMessages || !clamped.HasOlder {
		t.Fatalf("a limit above the cap was not clamped to %d: got %d, hasOlder %v", maxBeekeeperMessages, len(clamped.Messages), clamped.HasOlder)
	}
	if clamped.Messages[0].Text != fmt.Sprintf("bulk reply %d", bulk-maxBeekeeperMessages) {
		t.Fatalf("clamped window is not the most recent %d: %+v", maxBeekeeperMessages, clamped.Messages[0])
	}
}

// Both Beekeeper endpoints work through the HTTP layer with no owner
// project registered anywhere in the service. Posting an owner message
// records it in the Beekeeper thread and runs a Beekeeper turn on a fake
// model session, and reading the chat afterward, through the same HTTP
// layer, returns it durably recorded rather than merely echoed in the post
// response. Neither endpoint path names a project.
func TestBeekeeperHandlersWithNoProjectRegistered(t *testing.T) {
	t.Parallel()
	opts, _ := projectFixture(t)
	calls := make(chan coreadapter.PreparedTurn, 1)
	opts.BeekeeperTurns = turnsFunc(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		calls <- p
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "session"}, FinalResponse: "All clear."}, nil
	})
	_, c := start(t, opts)
	ctx := context.Background()

	out, err := c.SendBeekeeper(ctx, "Status please.")
	must(t, err)
	if len(out.Messages) != 2 || out.Messages[0].Author.Kind != beekeeper.AuthorOwner || out.Messages[0].Text != "Status please." ||
		out.Messages[1].Author.Kind != beekeeper.AuthorBeekeeper || out.Messages[1].Text != "All clear." {
		t.Fatalf("post response: %+v", out)
	}
	select {
	case p := <-calls:
		if p.Prompt != "Status please." {
			t.Fatalf("prepared turn: %+v", p)
		}
	default:
		t.Fatal("the beekeeper turn did not reach the model session")
	}

	// Durably recorded, not just echoed in the post response: read it back
	// through the GET endpoint, the same HTTP layer, with the same project
	// still unregistered.
	read, err := c.Beekeeper(ctx, 10)
	must(t, err)
	if len(read.Messages) != 2 || read.Messages[0].Text != "Status please." || read.Messages[1].Text != "All clear." {
		t.Fatalf("beekeeper chat after posting: %+v", read)
	}
}

// Posting while the Beekeeper's first turn is still in flight returns a
// conflict status through the HTTP layer and records nothing. The first
// turn's model session blocks on a channel until released, so the second
// post is deterministically issued while the first is genuinely in flight,
// never before it starts and never after it finishes.
func TestPostBeekeeperHandlerWhileBusyReturnsConflict(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	opts.BeekeeperTurns = turnsFunc(func(_ context.Context, _ coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		close(entered)
		<-release
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "session"}, FinalResponse: "All clear."}, nil
	})
	s, c := start(t, opts)
	ctx := context.Background()

	first := make(chan error, 1)
	go func() {
		_, err := c.SendBeekeeper(ctx, "First message")
		first <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first turn never started")
	}

	_, err := c.SendBeekeeper(ctx, "New message")
	api := beekeeperAPIError(t, err)
	if api.Code != Conflict {
		t.Fatalf("busy post: %+v", api)
	}

	repo := s.Beekeeper()
	th, err := repo.Thread(config.BeekeeperWorkstreamID, beekeeper.AgentID)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.Prompt != "First message" {
		t.Fatalf("a message was recorded while busy: %+v", th.Turns)
	}

	close(release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("the first post failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first turn never finished")
	}
}

// The shadow project and its reserved workstream appear in no project or
// workstream list the HTTP API serves.
func TestShadowProjectOmittedFromAPILists(t *testing.T) {
	t.Parallel()
	_, c := start(t, fixture(t))
	ctx := context.Background()

	cfg, err := c.Configuration(ctx)
	must(t, err)
	for _, p := range cfg.Projects {
		if p.ID == config.ShadowProjectID {
			t.Fatalf("configuration lists the shadow project: %+v", cfg.Projects)
		}
	}

	rt, err := c.Runtime(ctx)
	must(t, err)
	for _, p := range rt.Projects {
		if p.Project == config.ShadowProjectID {
			t.Fatalf("runtime lists the shadow project: %+v", rt.Projects)
		}
	}

	status, err := c.Statuses(ctx)
	must(t, err)
	for _, w := range status.Workstreams {
		if w.Project == config.ShadowProjectID || w.Workstream == config.BeekeeperWorkstreamID {
			t.Fatalf("status lists the shadow project or its workstream: %+v", status.Workstreams)
		}
	}
}

// Every project-scoped HTTP path given the shadow project's or its reserved
// workstream's identifier responds exactly as it does for an unknown
// project or workstream: the same API error code, which Service.fail and
// Service.failWith map to the same HTTP status.
func TestShadowProjectTreatedAsUnknownOnProjectScopedPaths(t *testing.T) {
	t.Parallel()
	_, c := start(t, fixture(t))
	ctx := context.Background()
	const unknownProject config.ProjectID = "p_ffffffffffffffffffffffffffffffff"
	const unknownWorkstream config.WorkstreamID = "w_ffffffffffffffffffffffffffffffff"

	projectPaths := []string{Prefix + "/projects/charter/%s", Prefix + "/projects/memory/%s"}
	for _, tmpl := range projectPaths {
		shadowErr := c.Do(ctx, "GET", fmt.Sprintf(tmpl, config.ShadowProjectID), nil, new(map[string]any))
		unknownErr := c.Do(ctx, "GET", fmt.Sprintf(tmpl, unknownProject), nil, new(map[string]any))
		shadowAPI, unknownAPI := beekeeperAPIError(t, shadowErr), beekeeperAPIError(t, unknownErr)
		if shadowAPI.Code != unknownAPI.Code {
			t.Fatalf("%s: shadow project %+v, unknown project %+v", tmpl, shadowAPI, unknownAPI)
		}
	}

	workstreamPaths := []string{Prefix + "/status/%s", Prefix + "/conversation/%s", Prefix + "/feed/%s", Prefix + "/base/%s", Prefix + "/documents/%s", Prefix + "/packet/%s", Prefix + "/delivery/%s", Prefix + "/trace/%s"}
	for _, tmpl := range workstreamPaths {
		shadowErr := c.Do(ctx, "GET", fmt.Sprintf(tmpl, config.BeekeeperWorkstreamID), nil, new(map[string]any))
		unknownErr := c.Do(ctx, "GET", fmt.Sprintf(tmpl, unknownWorkstream), nil, new(map[string]any))
		shadowAPI, unknownAPI := beekeeperAPIError(t, shadowErr), beekeeperAPIError(t, unknownErr)
		if shadowAPI.Code != unknownAPI.Code {
			t.Fatalf("%s: shadow workstream %+v, unknown workstream %+v", tmpl, shadowAPI, unknownAPI)
		}
	}
}
