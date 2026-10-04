package beekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func profile() coreadapter.Profile {
	return coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}
}

// request builds the owner's turn request for the Beekeeper's thread, the
// same shape conversation.go builds for a chief of staff.
func request(id string, ts time.Time, text string) trace.TurnRequest {
	return trace.TurnRequest{
		Header:  trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + id, Revision: 1, Project: config.ShadowProjectID, Workstream: config.BeekeeperWorkstreamID, At: ts, Actor: OwnerActor, Cause: "owner-message"},
		AgentID: AgentID, ThreadID: ThreadID, TurnID: "message_" + id, Profile: profile(), Prompt: text,
	}
}

// completeTurn claims, captures and completes the owner's request with a
// Beekeeper reply, using only trace.Repository's generic turn functions.
func completeTurn(t *testing.T, repo *trace.Repository, req trace.TurnRequest, ts time.Time, reply string) trace.QueuedTurn {
	t.Helper()
	ctx := context.Background()
	q, err := repo.ClaimTurn(ctx, config.BeekeeperWorkstreamID, AgentID, "token-"+req.TurnID, "/owned/session/"+req.TurnID, ts)
	if err != nil {
		t.Fatal(err)
	}
	h := req.Header
	h.Schema, h.ID, h.At, h.Actor = "osmia.trace.turn-response", "response_"+req.TurnID, ts.Add(time.Second), ReplyActor
	resp := trace.TurnResponse{
		Header: h, AgentID: AgentID, ThreadID: ThreadID, TurnID: req.TurnID, RequestID: req.ID, RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session-" + req.TurnID}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: ts, Duration: time.Second, FinalResponse: reply},
	}
	if err := repo.CaptureTurn(ctx, q.Claim.Token, resp); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteTurn(ctx, config.BeekeeperWorkstreamID, AgentID, req.TurnID, q.Claim.Token, ts.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	q.Response, q.CompletedAt = &resp, ts.Add(2*time.Second)
	return q
}

// The Beekeeper's thread is created and written only through
// trace.Repository's generic thread functions: there is exactly one
// Beekeeper, and opening the shadow project again, as either registered
// project's own activity would, finds the very same thread. Nothing is
// written into either registered project's trace or target repository.
func TestBeekeeperThreadIsSharedAndIsolatedFromRegisteredProjects(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	must(t, err)
	topPath, err := root.Config()
	must(t, err)
	must(t, os.MkdirAll(filepath.Dir(topPath), 0700))
	must(t, os.WriteFile(topPath, []byte("version = 1\nactive_projects = []\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))

	projects := []struct {
		id     config.ProjectID
		stream config.WorkstreamID
		name   string
	}{{projectA, streamA, "alpha"}, {projectB, streamB, "beta"}}
	clones := map[config.ProjectID]string{}
	for _, p := range projects {
		clone := filepath.Join(base, string(p.id))
		gitInit(t, clone)
		clones[p.id] = clone
		projectPath, err := root.ProjectConfig(p.id)
		must(t, err)
		must(t, config.WriteProjectConfig(projectPath, config.Project{Name: p.name, Upstream: "owner/" + p.name, Clone: clone, BaseBranch: "main"}))
		must(t, config.AddActiveProject(topPath, p.id))
		repo, err := trace.Create(ctx, root, config.Project{ID: p.id, Clone: clone}, at, owner)
		must(t, err)
		must(t, repo.CreateWorkstream(ctx, p.stream, at, owner))
		must(t, repo.Close())
	}
	cfg, err := config.Load(config.Options{Root: root.String()})
	must(t, err)
	if cfg.Hearsay.URL != "" {
		t.Fatalf("test assumes Hearsay is not configured: %+v", cfg.Hearsay)
	}

	before := map[config.ProjectID]map[string]string{}
	for _, p := range cfg.Projects {
		dir, err := root.ProjectTrace(p.ID)
		must(t, err)
		before[p.ID] = snapshot(t, dir)
	}
	cloneBefore := map[config.ProjectID]map[string]string{}
	for id, clone := range clones {
		cloneBefore[id] = snapshot(t, clone)
	}

	shadow, err := Open(ctx, root, cfg.Beekeeper, at)
	must(t, err)
	if _, err := EnsureThread(ctx, shadow, at); err != nil {
		t.Fatal(err)
	}
	_, err = shadow.EnqueueTurn(ctx, request("shared", at, "Which workstreams are waiting on me?"))
	must(t, err)
	must(t, shadow.Close())

	// A fresh Open, as either project's own activity would make, finds the
	// very same thread: there is exactly one Beekeeper.
	reopened, err := Open(ctx, root, cfg.Beekeeper, at.Add(time.Hour))
	must(t, err)
	defer reopened.Close()
	th, err := reopened.Thread(config.BeekeeperWorkstreamID, AgentID)
	must(t, err)
	if len(th.Turns) != 1 || th.Turns[0].Request.Prompt != "Which workstreams are waiting on me?" {
		t.Fatalf("beekeeper thread not shared across projects: %+v", th)
	}
	shadowDir, err := root.ProjectTrace(config.ShadowProjectID)
	must(t, err)
	if _, err := os.Stat(filepath.Join(shadowDir, ".git")); err != nil {
		t.Fatalf("beekeeper thread not stored in the service state directory: %v", err)
	}
	for _, p := range cfg.Projects {
		dir, err := root.ProjectTrace(p.ID)
		must(t, err)
		if dir == shadowDir {
			t.Fatalf("project %s shares the shadow project's directory", p.ID)
		}
		if !reflect.DeepEqual(snapshot(t, dir), before[p.ID]) {
			t.Fatalf("project %s trace changed by the beekeeper thread", p.ID)
		}
	}
	for id, clone := range clones {
		if !reflect.DeepEqual(snapshot(t, clone), cloneBefore[id]) {
			t.Fatalf("project %s target repository changed by the beekeeper thread", id)
		}
	}
}

// An owner request and a Beekeeper response round-trip through a reopen of
// the shadow project exactly as after a restart, carrying the shadow
// project's identifier and the owner and Beekeeper actors.
func TestBeekeeperTurnRoundTripsThroughRestart(t *testing.T) {
	ctx := context.Background()
	root, err := config.ResolveRoot(filepath.Join(t.TempDir(), "osmia"), "")
	must(t, err)
	b := config.Beekeeper{Name: "Hive", Profile: "default", Sandbox: "none"}

	repo, err := Open(ctx, root, b, at)
	must(t, err)
	if _, err := EnsureThread(ctx, repo, at); err != nil {
		t.Fatal(err)
	}
	req := request("1", at, "List the busy workstreams.")
	if _, err := repo.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	completeTurn(t, repo, req, at.Add(time.Second), "Workstream w_a is waiting on you.")
	must(t, repo.Close())

	reopened, err := Open(ctx, root, b, at.Add(time.Hour))
	must(t, err)
	defer reopened.Close()
	th, err := reopened.Thread(config.BeekeeperWorkstreamID, AgentID)
	must(t, err)
	if len(th.Turns) != 1 {
		t.Fatalf("turns: %+v", th.Turns)
	}
	got := th.Turns[0]
	if got.Request.Project != config.ShadowProjectID || got.Request.Actor != OwnerActor {
		t.Fatalf("owner request not intact: %+v", got.Request)
	}
	if got.Response == nil || got.Response.Project != config.ShadowProjectID || got.Response.Actor != ReplyActor {
		t.Fatalf("beekeeper response not intact: %+v", got.Response)
	}
	msgs, hasOlder, err := Messages(reopened, 50)
	must(t, err)
	if hasOlder {
		t.Fatal("unexpected older messages")
	}
	if len(msgs) != 2 || msgs[0].Author.Kind != AuthorOwner || msgs[1].Author.Kind != AuthorBeekeeper {
		t.Fatalf("messages: %+v", msgs)
	}
	if msgs[0].Text != "List the busy workstreams." || msgs[1].Text != "Workstream w_a is waiting on you." {
		t.Fatalf("messages: %+v", msgs)
	}
}

// The Beekeeper's history is exactly what internal/thread's Replay produces
// for its thread, including the omitted-prefix note Replay records; no
// Beekeeper-specific windowing is applied.
func TestBeekeeperHistoryIsExactlyWhatReplayProduces(t *testing.T) {
	ctx := context.Background()
	root, err := config.ResolveRoot(filepath.Join(t.TempDir(), "osmia"), "")
	must(t, err)
	b := config.Beekeeper{Name: "Hive", Profile: "default", Sandbox: "none"}
	repo, err := Open(ctx, root, b, at)
	must(t, err)
	defer repo.Close()
	if _, err := EnsureThread(ctx, repo, at); err != nil {
		t.Fatal(err)
	}

	const turns = 5
	for i := 1; i <= turns; i++ {
		id := fmt.Sprintf("%d", i)
		ts := at.Add(time.Duration(i) * time.Minute)
		req := request(id, ts, "Status check "+id)
		if _, err := repo.EnqueueTurn(ctx, req); err != nil {
			t.Fatal(err)
		}
		completeTurn(t, repo, req, ts, "Reply "+id)
	}

	th, err := repo.Thread(config.BeekeeperWorkstreamID, AgentID)
	must(t, err)
	limits := thread.ReplayLimits{MaxTurns: 2}
	last := th.Turns[len(th.Turns)-1].Sequence
	history, from, omitted, err := thread.Replay(th, last+1, limits)
	must(t, err)
	if omitted != turns-2 {
		t.Fatalf("expected %d omitted turns, got %d", turns-2, omitted)
	}
	if from != th.Turns[turns-2].Sequence {
		t.Fatalf("unexpected replay start sequence: %d", from)
	}
	var decoded struct {
		Omitted   int `json:"omitted_prefix_exchanges"`
		Exchanges []struct {
			Sequence uint64 `json:"sequence"`
		} `json:"exchanges"`
	}
	must(t, json.Unmarshal([]byte(history), &decoded))
	if decoded.Omitted != turns-2 || len(decoded.Exchanges) != 2 {
		t.Fatalf("beekeeper history does not match Replay's own omission marker: %+v", decoded)
	}
}

// Messages returns the most recent messages oldest first, with each one's
// author and whether older messages exist, for the empty-thread, exactly-N
// and more-than-N cases.
func TestMessagesOldestFirstWithHasOlderFlag(t *testing.T) {
	ctx := context.Background()
	root, err := config.ResolveRoot(filepath.Join(t.TempDir(), "osmia"), "")
	must(t, err)
	b := config.Beekeeper{Name: "Hive", Profile: "default", Sandbox: "none"}
	repo, err := Open(ctx, root, b, at)
	must(t, err)
	defer repo.Close()

	empty, hasOlder, err := Messages(repo, 50)
	must(t, err)
	if len(empty) != 0 || hasOlder {
		t.Fatalf("empty thread: %v %v", empty, hasOlder)
	}

	if _, err := EnsureThread(ctx, repo, at); err != nil {
		t.Fatal(err)
	}
	const turns = 4
	for i := 1; i <= turns; i++ {
		id := fmt.Sprintf("%d", i)
		ts := at.Add(time.Duration(i) * time.Minute)
		req := request(id, ts, fmt.Sprintf("Question %d", i))
		if _, err := repo.EnqueueTurn(ctx, req); err != nil {
			t.Fatal(err)
		}
		completeTurn(t, repo, req, ts, fmt.Sprintf("Answer %d", i))
	}

	all, hasOlder, err := Messages(repo, turns*2)
	must(t, err)
	if hasOlder {
		t.Fatal("exactly-N case should report no older messages")
	}
	if len(all) != turns*2 {
		t.Fatalf("expected %d messages, got %d: %+v", turns*2, len(all), all)
	}
	for i := 0; i+1 < len(all); i += 2 {
		if all[i].Author.Kind != AuthorOwner || all[i+1].Author.Kind != AuthorBeekeeper {
			t.Fatalf("unexpected authors at %d: %+v", i, all[i:i+2])
		}
	}
	for i := 1; i < len(all); i++ {
		if all[i].At.Before(all[i-1].At) {
			t.Fatal("messages not oldest first")
		}
	}

	limited, hasOlder, err := Messages(repo, 3)
	must(t, err)
	if !hasOlder {
		t.Fatal("expected older messages to exist")
	}
	if len(limited) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(limited), limited)
	}
	if !reflect.DeepEqual(limited, all[len(all)-3:]) {
		t.Fatalf("limited window is not the most recent 3 messages: %+v", limited)
	}
}

// The Beekeeper's thread, including recording an owner message and reading
// it back, works with Hearsay not configured.
func TestBeekeeperThreadWorksWithHearsayNotConfigured(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	must(t, err)
	topPath, err := root.Config()
	must(t, err)
	must(t, os.MkdirAll(filepath.Dir(topPath), 0700))
	must(t, os.WriteFile(topPath, []byte("version = 1\nactive_projects = []\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))
	cfg, err := config.Load(config.Options{Root: root.String()})
	must(t, err)
	if cfg.Hearsay.URL != "" || len(cfg.Hearsay.Agents) != 0 {
		t.Fatalf("test assumes Hearsay is not configured: %+v", cfg.Hearsay)
	}

	repo, err := Open(ctx, root, cfg.Beekeeper, at)
	must(t, err)
	defer repo.Close()
	if _, err := EnsureThread(ctx, repo, at); err != nil {
		t.Fatal(err)
	}
	req := request("1", at, "Are you there?")
	if _, err := repo.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	msgs, hasOlder, err := Messages(repo, 10)
	must(t, err)
	if hasOlder || len(msgs) != 1 || msgs[0].Author.Kind != AuthorOwner || msgs[0].Text != "Are you there?" {
		t.Fatalf("messages: %+v %v", msgs, hasOlder)
	}
}

// The role prompt states the Beekeeper's two capabilities, that replies from
// chiefs of staff arrive on its next turn, and that it holds no owner gates.
func TestRolePromptStatesCapabilitiesAndLimits(t *testing.T) {
	lower := strings.ToLower(RolePrompt)
	for _, want := range []string{
		"owner's one assistant for the whole factory",
		"list every registered project",
		"message the chief of staff",
		"at the start of your next turn",
		"none of the owner's gates",
	} {
		if !strings.Contains(lower, want) {
			t.Fatalf("role prompt missing %q: %s", want, RolePrompt)
		}
	}
}
