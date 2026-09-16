package status

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func valid() trace.StatusContent {
	return trace.StatusContent{
		Goal:      "Add resumable uploads to the storage client.",
		Attention: "Decide whether retries may reorder parts.",
		Note:      "The plan is drafted. The reviewers asked for a clearer rollback story, and the architect is revising it.",
		Agents:    []string{"The architect is revising the plan.", "A reviewer is reading the upload changes."},
	}
}

func TestCheckAcceptsPlainStatus(t *testing.T) {
	for name, edit := range map[string]func(*trace.StatusContent){
		"complete":      func(*trace.StatusContent) {},
		"no attention":  func(c *trace.StatusContent) { c.Attention = "" },
		"no agents":     func(c *trace.StatusContent) { c.Agents = []string{} },
		"slashed prose": func(c *trace.StatusContent) { c.Note = "Reads and/or writes are affected. Timing is 24/7." },
		"dates and hex words": func(c *trace.StatusContent) {
			c.Note = "The review of 2026-09-16 found a defaced banner. It is fixed."
		},
		"ordinary known word":     func(c *trace.StatusContent) { c.Note = "The first review passed." },
		"version number":          func(c *trace.StatusContent) { c.Note = "Version 1.5 of the protocol is supported." },
		"large number":            func(c *trace.StatusContent) { c.Note = "Uploads of 10485760 bytes resume." },
		"words around a known ID": func(c *trace.StatusContent) { c.Note = "The xchief_threadx and chief_thread2 labels are prose here." },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid()
			edit(&c)
			if err := Check(c, []string{"first", "", "test", "chief_thread"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckRejects(t *testing.T) {
	known := []string{"p_00000000000000000000000000000001", "chief_thread", "turn-9", "backend-session-identifier"}
	cases := []struct {
		name   string
		edit   func(*trace.StatusContent)
		reason string
	}{
		{"empty goal", func(c *trace.StatusContent) { c.Goal = " " }, "goal is required"},
		{"empty note", func(c *trace.StatusContent) { c.Note = "" }, "note is required"},
		{"missing agents", func(c *trace.StatusContent) { c.Agents = nil }, "agents is required"},
		{"empty agent line", func(c *trace.StatusContent) { c.Agents = []string{"The architect.", " "} }, "agents[1] is empty"},
		{"multi-sentence goal", func(c *trace.StatusContent) { c.Goal = "Add uploads. Then add downloads." }, "goal must be one sentence, not 2"},
		{"exclaimed goal", func(c *trace.StatusContent) { c.Goal = "Add uploads! Fast?" }, "goal must be one sentence"},
		{"multi-line goal", func(c *trace.StatusContent) { c.Goal = "Add uploads\nnow" }, "goal must be a single line"},
		{"long goal", func(c *trace.StatusContent) { c.Goal = strings.Repeat("a", 201) }, "goal must be at most 200"},
		{"multi-line attention", func(c *trace.StatusContent) { c.Attention = "Decide.\nNow." }, "attention must be a single line"},
		{"long note", func(c *trace.StatusContent) { c.Note = strings.Repeat("word ", 241) }, "note must be at most 1200"},
		{"too many note sentences", func(c *trace.StatusContent) { c.Note = strings.Repeat("It moved. ", 7) }, "note must be a few sentences"},
		{"multi-line agent", func(c *trace.StatusContent) { c.Agents = []string{"The architect\nis busy."} }, "agents[0] must be a single line"},
		{"too many agents", func(c *trace.StatusContent) { c.Agents = make([]string, 33) }, "at most 32"},
		{"short commit hash", func(c *trace.StatusContent) { c.Note = "Landed a1b2c3d on the branch." }, `note contains a commit hash ("a1b2c3d")`},
		{"full commit hash", func(c *trace.StatusContent) {
			c.Attention = "Review 0123456789abcdef0123456789abcdef01234567 today."
		}, "attention contains a commit hash"},
		{"branch ref", func(c *trace.StatusContent) { c.Note = "Work continues on feature/uploads." }, `note contains a branch name or file path ("feature/uploads")`},
		{"remote branch", func(c *trace.StatusContent) { c.Agents = []string{"A mason is rebasing onto origin/main."} }, "agents[0] contains a branch name or file path"},
		{"relative path", func(c *trace.StatusContent) { c.Note = "The change touches internal/storage/upload.go only." }, "note contains a branch name or file path"},
		{"absolute path", func(c *trace.StatusContent) { c.Note = "Logs are in /var/log." }, `note contains a file path ("/var/log")`},
		{"home path", func(c *trace.StatusContent) { c.Note = "See ~/notes for details." }, "note contains a file path"},
		{"file name", func(c *trace.StatusContent) { c.Goal = "Rewrite upload.go for retries." }, `goal contains a file name ("upload.go")`},
		{"url", func(c *trace.StatusContent) { c.Note = "See https://example.com." }, "note contains a URL"},
		{"uuid session", func(c *trace.StatusContent) {
			c.Agents = []string{"Session 123e4567-e89b-12d3-a456-426614174000 is running."}
		}, "agents[0] contains a session ID"},
		{"service session token", func(c *trace.StatusContent) { c.Note = "Claim ABCDEFGHIJKLMNOPQRSTUVWXYZ expired." }, "note contains a session ID"},
		{"prefixed session", func(c *trace.StatusContent) { c.Note = "Resumed ses_4f9aZ21 after restart." }, "note contains a session ID"},
		{"known session", func(c *trace.StatusContent) { c.Note = "Resumed backend-session-identifier after restart." }, `note contains an Osmia or backend identifier ("backend-session-identifier")`},
		{"known thread", func(c *trace.StatusContent) { c.Agents = []string{"chief_thread is idle."} }, "agents[0] contains an Osmia or backend identifier"},
		{"known turn", func(c *trace.StatusContent) { c.Note = "It finished (turn-9)." }, `note contains an Osmia or backend identifier ("turn-9")`},
		{"project ID", func(c *trace.StatusContent) { c.Note = "Project p_00000000000000000000000000000001 is active." }, "note contains an Osmia or backend identifier"},
		{"workstream ID", func(c *trace.StatusContent) { c.Goal = "Finish w_0123456789abcdef0123456789abcdef." }, "goal contains an Osmia project, workstream or thread ID"},
		{"generated thread ID", func(c *trace.StatusContent) { c.Note = "Thread t_9f8e7d6c5b4a is waiting." }, "note contains an Osmia project, workstream or thread ID"},
		{"versioned model", func(c *trace.StatusContent) { c.Agents = []string{"The mason runs on claude-opus-5."} }, `agents[0] contains a model name ("claude-opus-5")`},
		{"spaced model", func(c *trace.StatusContent) { c.Note = "Switched to GPT 5 for reviews." }, `note contains a model name ("GPT 5")`},
		{"family name", func(c *trace.StatusContent) { c.Note = "Sonnet is reviewing the change." }, `note contains a model name ("Sonnet")`},
		{"o-series model", func(c *trace.StatusContent) { c.Note = "The reviewer uses o3-mini now." }, `note contains a model name ("o3-mini")`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.edit(&c)
			err := Check(c, known)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("got %v, want %q", err, tc.reason)
			}
		})
	}
}

// chief creates a trace with a chief-of-staff thread whose turn is claimed.
func chief(t *testing.T) (*trace.Repository, coreadapter.Scope, config.Root, config.Project) {
	t.Helper()
	ctx := context.Background()
	base, err := os.MkdirTemp("", "st-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	root, err := config.ResolveRoot(filepath.Join(base, "root"), "")
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(base, "clone")
	cmd := exec.Command("git", "init", "--quiet", clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	p := config.Project{ID: "p_00000000000000000000000000000001", Clone: clone}
	stream := config.WorkstreamID("w_00000000000000000000000000000001")
	at := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, root, p, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	h := trace.Header{Schema: "osmia.trace.agent", Version: 1, ID: "chief", Revision: 1, Project: p.ID, Workstream: stream, At: at, Actor: owner, Cause: "workstream-create"}
	must(t, repo.CreateWorkstream(ctx, stream, at, owner))
	must(t, repo.CreateThread(ctx, trace.Agent{Header: h, Role: "chief_of_staff", ThreadID: "chief_thread"}))
	h.Schema, h.ID, h.Cause, h.Depth = "osmia.trace.turn-request", "request_one", "message_one", 1
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: "chief", ThreadID: "chief_thread", TurnID: "one", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, Prompt: "Owner message"})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, "chief", "token", "/owned", at)
	must(t, err)
	return repo, coreadapter.Scope{Project: string(p.ID), Workstream: string(stream), Thread: "chief_thread", Turn: "one", Role: "chief_of_staff"}, root, p
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func clock() time.Time { return time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC) }

func TestToolStoresReplacesAndRejects(t *testing.T) {
	ctx := context.Background()
	repo, scope, root, p := chief(t)
	for _, role := range []string{"mason", "reviewer", "architect", ""} {
		s := scope
		s.Role = role
		if _, err := Tool(repo, "chief", s, clock); err == nil {
			t.Errorf("%q received %s", role, ToolName)
		}
	}
	if _, err := Tool(nil, "chief", scope, clock); err == nil {
		t.Error("tool without a trace")
	}
	tool, err := Tool(repo, "chief", scope, clock)
	must(t, err)
	if tool.Name != ToolName || tool.Effect != coreadapter.ToolMemory || !json.Valid(tool.InputSchema) {
		t.Fatalf("tool: %+v", tool)
	}
	call := func(input string) string {
		t.Helper()
		out, err := tool.Handle(ctx, json.RawMessage(input))
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		return string(out)
	}
	rejected := call(`{"goal":"Ship it. Then more.","note":"n","agents":[]}`)
	if rejected != `{"stored":false,"reason":"goal must be one sentence, not 2"}` {
		t.Fatalf("rejection: %s", rejected)
	}
	rejected = call(`{"goal":"Ship it.","note":"Waiting on chief_thread.","agents":[]}`)
	if !strings.Contains(rejected, `"stored":false`) || !strings.Contains(rejected, "chief_thread") {
		t.Fatalf("known identifier: %s", rejected)
	}
	for _, input := range []string{`{"goal":"g","note":"n","agents":[],"role":"mason"}`, `{"goal":"g"}{}`, `[]`} {
		if _, err := tool.Handle(ctx, json.RawMessage(input)); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	if out := call(`{"goal":"Ship uploads.","attention":"Approve the plan.","note":"The plan is ready.","agents":["The architect is idle."]}`); out != `{"stored":true,"revision":1}` {
		t.Fatalf("store: %s", out)
	}
	if out := call(`{"goal":"Ship uploads.","note":"The plan was approved.","agents":[]}`); out != `{"stored":true,"revision":2}` {
		t.Fatalf("replace: %s", out)
	}
	must(t, repo.Close())
	reopened, err := trace.Open(root, p)
	must(t, err)
	defer reopened.Close()
	list, err := reopened.Statuses()
	must(t, err)
	want := trace.StatusContent{Goal: "Ship uploads.", Note: "The plan was approved.", Agents: []string{}}
	if len(list) != 1 || list[0].Status == nil || list[0].Status.Revision != 2 || list[0].Status.Attention != "" ||
		list[0].Status.Goal != want.Goal || list[0].Status.Note != want.Note || len(list[0].Status.Agents) != 0 || !list[0].Status.At.Equal(clock()) {
		t.Fatalf("stored: %+v", list)
	}
}
