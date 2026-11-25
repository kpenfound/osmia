package clientturns

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
)

const startCommit = "0123456789abcdef0123456789abcdef01234567"

func decoded(t *testing.T, body string) string {
	t.Helper()
	var out string
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("tool answered %q: %v", body, err)
	}
	return out
}

func eventsOf(t *testing.T, r *rig, turn, kind string) []trace.ClientEvent {
	t.Helper()
	all, err := r.repo.ClientEvents(stream, turn)
	must(t, err)
	var out []trace.ClientEvent
	for _, e := range all {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func thread1(t *testing.T, r *rig) trace.Thread {
	t.Helper()
	th, err := r.repo.Thread(stream, "agent")
	must(t, err)
	return th
}

func TestARemoteMasonTurnRunsOnItsClientsViewAndItsFinishedViewIsCaptured(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRig(t, "mason")
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	done := r.run(ctx, "t1")

	offer := c.nextOffer()
	if offer.Turn != "t1" || offer.Role != "mason" || offer.Profile != "default" || offer.Settings.Backend != "fake" || offer.Project != string(project) || offer.Workstream != string(stream) {
		t.Fatalf("offer identity: %+v", offer)
	}
	if !slices.Equal(offer.Fallbacks, []string{"fallback"}) || offer.Prompt != "Do the work for t1" || !strings.Contains(offer.SystemPrompt, "Role instructions") {
		t.Fatalf("offer profile and prompts: %+v", offer)
	}
	wantView := api.TurnView{Kind: api.ViewGit, Project: string(project), Commit: startCommit, Paths: []string{"src", "docs"}}
	if !reflect.DeepEqual(offer.View, wantView) || !offer.ReturnView {
		t.Fatalf("view: %+v", offer.View)
	}
	if offer.ToolEndpoint != api.MCPPath("t1") || offer.ToolToken != "token-t1" || !offer.Capabilities.WriteFiles || !slices.Contains(offer.Capabilities.Tools, "file_write") {
		t.Fatalf("tools: %q %q %+v", offer.ToolEndpoint, offer.ToolToken, offer.Capabilities)
	}
	if offer.Execution.DaggerCloudTokenEnv != "" || len(offer.Environment) != 0 {
		t.Fatalf("the offer carries a secret or an environment: %+v", offer)
	}
	c.claim(offer, map[string]string{"src/main.go": "package main\n", "docs/notes.md": "# notes\n"})

	put(t, c.view, "src/main.go", "package main\n// the session changed this file in its own view\n")
	status, body := r.relay("t1", offer.ToolToken, "file_read", `{"path":"src/main.go"}`)
	if status != 200 || decoded(t, body) != "package main\n// the session changed this file in its own view\n" {
		t.Fatalf("a relayed read did not return the file as the session changed it: %d %s", status, body)
	}
	if status, body = r.relay("t1", offer.ToolToken, "file_write", `{"path":"src/new.go","content":"package main\n"}`); status != 200 {
		t.Fatalf("relayed write: %d %s", status, body)
	}
	if got := read(t, c.view, "src/new.go"); got != "package main\n" {
		t.Fatalf("the relayed write is not in the client's view: %q", got)
	}
	if status, _ = r.relay("t1", offer.ToolToken, "file_read", `{"path":"../outside"}`); status == 200 {
		t.Fatal("a path outside the view was read")
	}
	if status, _ = r.relay("t1", offer.ToolToken, "file_write", `{"path":".git/config","content":"x"}`); status == 200 {
		t.Fatal("VCS metadata was written")
	}
	for _, read := range c.reads {
		if strings.Contains(read, "..") {
			t.Fatalf("the client was asked for a path outside its view: %s", read)
		}
	}
	if audits := r.audits(); len(audits) != 4 || !strings.HasPrefix(audits[0], "t1 file_read ") || !strings.HasPrefix(audits[1], "t1 file_write ") {
		t.Fatalf("each call is audited on the server, refused ones too: %q", audits)
	}
	if entries, err := os.ReadDir(filepath.Join(r.root.String(), "build")); err != nil || len(entries) != 0 {
		t.Fatalf("the server built a view for a turn the client builds its own for: %v %v", entries, err)
	}

	answer, refusal := c.upload("t1", offer.ToolToken)
	if refusal != nil || answer != api.ViewAccepted {
		t.Fatalf("view upload: %q %v", answer, refusal)
	}
	if answer, err := c.result("t1", complete("done")); err != nil || answer != api.ViewAccepted {
		t.Fatalf("result: %q %v", answer, err)
	}
	out := waitFor(t, done, "the turn")
	if out.err != nil || out.turn.Response == nil || out.turn.CompletedAt.IsZero() {
		t.Fatalf("turn: %+v %v", out.turn, out.err)
	}

	captures := r.captures()
	if len(captures) != 1 || captures[0].scope.Turn != "t1" {
		t.Fatalf("captures: %+v", captures)
	}
	want := map[string]string{"src/main.go": "package main\n// the session changed this file in its own view\n", "src/new.go": "package main\n", "docs/notes.md": "# notes\n"}
	if !reflect.DeepEqual(captures[0].files, want) {
		t.Fatalf("captured %v, want %v", captures[0].files, want)
	}
	local := t.TempDir()
	for name, data := range want {
		put(t, local, name, data)
	}
	view, err := isolation.Views{Directory: filepath.Join(r.root.String(), "build")}.Create(ctx, coreadapter.Workspace{Directory: local, Access: coreadapter.ReadWrite}, []string{"src", "docs"})
	must(t, err)
	must(t, r.turns.Capture(ctx, captures[0].scope, view, coreadapter.SessionResult{}))
	must(t, view.Release(ctx))
	if again := r.captures(); len(again) != 2 || !reflect.DeepEqual(again[1].files, captures[0].files) {
		t.Fatalf("the same files captured locally differ from the returned view: %+v", again)
	}

	response := thread1(t, r).Turns[0].Response
	if response.Result.Client != "node-a" || response.Result.ToolCounts["file_read"] != 2 || response.Result.ToolCounts["file_write"] != 2 || response.Result.SessionDirectory != filepath.Join(r.root.String(), "sessions", "t1") {
		t.Fatalf("recorded result: %+v", response.Result)
	}
	if holder, ok := thread1(t, r).SessionClient(); !ok || holder != "node-a" {
		t.Fatalf("session holder: %q %v", holder, ok)
	}
	if len(eventsOf(t, r, "t1", trace.ClientPlaced)) != 1 || len(eventsOf(t, r, "t1", trace.ClientReturned)) != 2 {
		t.Fatalf("events: %+v", eventsOf(t, r, "t1", ""))
	}
}

func TestATraceDocumentViewReachesTheClientAsAnArchiveAndItsResultIsRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRig(t, "architect")
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	done := r.run(ctx, "t1")

	offer := c.nextOffer()
	if offer.View.Kind != api.ViewArchive || offer.View.Commit != "" || offer.ReturnView || offer.Role != "architect" {
		t.Fatalf("offer: %+v", offer)
	}
	c.claim(offer, nil)
	if got := read(t, c.view, "docs/notes.md"); got != "# notes\n" {
		t.Fatalf("archive content: %q", got)
	}
	if got := read(t, c.view, "src/main.go"); got != "package main\n" {
		t.Fatalf("archive content: %q", got)
	}
	if _, refusal := c.upload("t1", offer.ToolToken); refusal == nil || refusal.Code != api.NotFound {
		t.Fatalf("a read-only turn took a view: %v", refusal)
	}
	if status, _ := r.relay("t1", offer.ToolToken, "file_write", `{"path":"docs/x.md","content":"x"}`); status == 200 {
		t.Fatal("a read-only turn wrote a file")
	}
	if answer, err := c.result("t1", complete("designed")); err != nil || answer != api.ViewAccepted {
		t.Fatalf("result: %q %v", answer, err)
	}
	out := waitFor(t, done, "the turn")
	if out.err != nil || out.turn.Response == nil || out.turn.Response.Result.Outcome == nil || out.turn.Response.Result.Outcome.Status != "complete" || out.turn.Response.Result.Client != "node-a" {
		t.Fatalf("turn: %+v %v", out.turn.Response, out.err)
	}
	if len(r.captures()) != 0 {
		t.Fatal("a read-only turn was captured")
	}
	if entries, _ := os.ReadDir(filepath.Join(r.root.String(), "build")); len(entries) != 0 {
		t.Fatalf("the server's copy of the archived view was not removed: %v", entries)
	}
}

func TestARelayedCallNeedsTheTurnsOwnToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRig(t, "mason")
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	done := r.run(ctx, "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})

	if status, body := r.relay("t1", offer.ToolToken, "note", `{"text":"hello"}`); status != 200 || !strings.Contains(decoded(t, body), "noted") {
		t.Fatalf("relayed call: %d %s", status, body)
	}
	hits := r.up.calls("t1")
	for name, token := range map[string]string{"another turn's token": "token-t2", "no token": "", "a wrong token": "nope"} {
		if status, _ := r.relay("t1", token, "note", `{}`); status == 200 {
			t.Fatalf("a call with %s was handled", name)
		}
	}
	if status, _ := r.relay("t9", "token-t9", "note", `{}`); status == 200 {
		t.Fatal("a call for a turn that was never offered was handled")
	}
	if r.up.calls("t1") != hits {
		t.Fatal("a refused call reached the turn's endpoint")
	}
	if audits := r.audits(); len(audits) != 1 || !strings.HasPrefix(audits[0], "t1 note ") {
		t.Fatalf("audits: %q", audits)
	}
	_, _ = c.upload("t1", offer.ToolToken)
	_, err := c.result("t1", complete("done"))
	must(t, err)
	waitFor(t, done, "the turn")
	if status, _ := r.relay("t1", offer.ToolToken, "note", `{}`); status == 200 {
		t.Fatal("the relay still answers a turn that ended")
	}
}

func TestASkillReadIsServedFromTheClientsPreparedSkills(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRig(t, "mason")
	r.backend, r.skills = "claude", []string{"owner/skill"}
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	put(t, c.skills, "demo/SKILL.md", "# the client's own skill\n")
	r.queue("t1")
	done := r.run(ctx, "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})
	if !slices.Equal(offer.Execution.Skills, []string{"owner/skill"}) {
		t.Fatalf("skills: %+v", offer.Execution)
	}
	if status, body := r.relay("t1", offer.ToolToken, "file_read", `{"path":"/skills/demo/SKILL.md"}`); status != 200 || decoded(t, body) != "# the client's own skill\n" {
		t.Fatalf("skill read: %d %s", status, body)
	}
	if status, _ := r.relay("t1", offer.ToolToken, "file_read", `{"path":"/skills/../../etc/passwd"}`); status == 200 {
		t.Fatal("a skill path climbed out")
	}
	if !slices.Contains(c.reads, "skill_read /skills/demo/SKILL.md") || slices.ContainsFunc(c.reads, func(s string) bool { return strings.Contains(s, "..") }) {
		t.Fatalf("the client was asked for: %q", c.reads)
	}
	_, _ = c.upload("t1", offer.ToolToken)
	res := complete("done")
	res.Session.Backend = "claude"
	_, err := c.result("t1", res)
	must(t, err)
	waitFor(t, done, "the turn")
}

func TestHeartbeatProgressReachesTheActivityRegistryAndStallsAreFlagged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRig(t, "mason")
	c := r.client("node-a")
	c.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	done := r.run(ctx, "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})

	beat := func(phase string, progress float64) {
		t.Helper()
		_, err := r.fleet.Heartbeat(c.lease, c.node, api.HeartbeatRequest{Turns: []api.TurnProgress{{Turn: "t1", Phase: phase, Progress: &progress}}})
		must(t, err)
	}
	for deadline := time.Now().Add(5 * time.Second); len(r.acts.View()) == 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	entries := r.acts.View()
	if len(entries) != 1 || entries[0].Kind != api.ActivityClientTurn || entries[0].Workstream != stream || !strings.Contains(entries[0].Phase, "node-a") {
		t.Fatalf("activity before any heartbeat: %+v", entries)
	}
	beat("editing the handler", 0.25)
	entries = r.acts.View()
	if len(entries) != 1 || entries[0].Phase != "editing the handler" || entries[0].Progress == nil || *entries[0].Progress != 0.25 {
		t.Fatalf("activity: %+v", entries)
	}
	if status, _ := r.acts.Status(); status != api.BackendReady {
		t.Fatalf("status after progress: %s", status)
	}
	r.clock.advance(20 * time.Second)
	beat("editing the handler", 0.25)
	r.clock.advance(20 * time.Second)
	beat("editing the handler", 0.25)
	status, cause := r.acts.Status()
	if status != api.BackendDegraded || !strings.Contains(cause, "no progress") || !strings.Contains(cause, "editing the handler") {
		t.Fatalf("a turn whose heartbeats carry no new progress was not flagged: %s %q", status, cause)
	}
	beat("running the checks", 0.5)
	if status, _ := r.acts.Status(); status != api.BackendReady {
		t.Fatalf("progress did not clear the stall: %s", status)
	}
	_, _ = c.upload("t1", offer.ToolToken)
	_, err := c.result("t1", complete("done"))
	must(t, err)
	waitFor(t, done, "the turn")
	if len(r.acts.View()) != 0 {
		t.Fatalf("the finished turn is still listed: %+v", r.acts.View())
	}
}

func TestAResultIsTakenFromTheTurnsClientAndItsSessionFilesComeFromIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRig(t, "mason")
	c, other := r.client("node-a"), r.client("node-b")
	c.connect(api.ClientConnectRequest{})
	other.connect(api.ClientConnectRequest{})
	c.serve()
	r.place(c.node)
	r.queue("t1")
	done := r.run(ctx, "t1")
	offer := c.nextOffer()
	c.claim(offer, map[string]string{"src/main.go": "x"})
	_, _ = c.upload("t1", offer.ToolToken)

	if _, err := other.result("t1", complete("done")); err == nil || !strings.Contains(err.Error(), "another client") {
		t.Fatalf("another node returned the turn's result: %v", err)
	}
	if _, err := c.result("t1", complete("done"), api.SessionFile{Name: "../escape", Data: []byte("x")}); err == nil {
		t.Fatal("a session file outside the session directory was taken")
	}
	if answer, err := c.result("t1", complete("done"), api.SessionFile{Name: "sandbox.name", Data: []byte("sbx-1")}); err != nil || answer != api.ViewAccepted {
		t.Fatalf("result: %q %v", answer, err)
	}
	waitFor(t, done, "the turn")
	if got := read(t, filepath.Join(r.root.String(), "sessions", "t1"), "sandbox.name"); got != "sbx-1" {
		t.Fatalf("session file: %q", got)
	}
}
