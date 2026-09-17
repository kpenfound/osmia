package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// answering is a fake chief of staff that answers every prompt at once.
type answering struct{}

func (answering) Run(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "claude", ID: "session"}, FinalResponse: "Noted.\nThe plan is next."}, nil
}

// ticking returns a clock that starts at written and advances a minute per read.
func ticking() func() time.Time {
	var mu sync.Mutex
	now := written
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Minute)
		return now
	}
}

// withConversation creates the project trace with two workstreams and runs
// chief-of-staff turns with the fake chief of staff.
func withConversation(t *testing.T) service.Options {
	t.Helper()
	ctx := context.Background()
	opts := fixture(t)
	cfg, err := config.Load(opts.Config)
	must(t, err)
	cmd := exec.Command("git", "init", "--quiet", cfg.Project.Clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, written, owner)
	must(t, err)
	must(t, repo.CreateWorkstream(ctx, stream, written, owner))
	must(t, repo.CreateWorkstream(ctx, quiet, written, owner))
	must(t, repo.Close())
	opts.Reconciliation.Now = ticking()
	opts.Threads = func(r *trace.Repository, _ *config.Config) (coreadapter.Reconciler, error) {
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: answering{}, Now: opts.Reconciliation.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(cfg.Root.String(), "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	return opts
}

func TestSendAndConversation(t *testing.T) {
	opts := withConversation(t)
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	root := opts.Config.Root

	var sent service.ConversationEntry
	must(t, json.Unmarshal([]byte(successful(t, root, "send", stream, "Start with uploads.", "--json")), &sent))
	if sent.Kind != "message" || sent.State != service.TurnQueued || sent.Text != "Start with uploads." || !strings.HasPrefix(sent.Turn, "message_") {
		t.Fatalf("send --json: %+v", sent)
	}
	var list service.ConversationResponse
	deadline := time.Now().Add(5 * time.Minute)
	for {
		must(t, json.Unmarshal([]byte(successful(t, root, "conversation", stream, "--json")), &list))
		if len(list.Entries) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no response: %+v", list)
		}
		time.Sleep(20 * time.Millisecond)
	}
	answered := list.Entries[1].At
	want := service.ConversationResponse{Workstream: stream, Entries: []service.ConversationEntry{
		{Turn: sent.Turn, Kind: "message", Text: "Start with uploads.", At: sent.At, State: service.TurnDone},
		{Turn: sent.Turn, Kind: "response", Text: "Noted.\nThe plan is next.", At: answered, State: service.TurnDone},
	}}
	if !reflect.DeepEqual(list, want) || !answered.After(sent.At) {
		t.Fatalf("conversation --json: %+v", list)
	}
	var raw struct {
		Workstream string                       `json:"workstream"`
		Entries    []map[string]json.RawMessage `json:"entries"`
	}
	must(t, json.Unmarshal([]byte(successful(t, root, "conversation", stream, "--json")), &raw))
	for _, e := range raw.Entries {
		keys := []string{}
		for k := range e {
			keys = append(keys, k)
		}
		if len(keys) != 5 || e["turn"] == nil || e["kind"] == nil || e["text"] == nil || e["at"] == nil || e["state"] == nil {
			t.Fatalf("entry keys: %v", keys)
		}
	}

	if want := "Conversation: " + stream + "\n" +
		sent.At.Format(time.RFC3339) + " owner [" + sent.Turn + " done]\n  Start with uploads.\n" +
		answered.Format(time.RFC3339) + " chief of staff [" + sent.Turn + " done]\n  Noted.\n  The plan is next.\n"; successful(t, root, "conversation", stream) != want {
		t.Fatalf("conversation:\n%s\nwant:\n%s", successful(t, root, "conversation", stream), want)
	}

	out := successful(t, root, "send", quiet, "Hold this one.")
	if !strings.HasPrefix(out, "Message message_") || !strings.HasSuffix(out, " sent to the chief of staff of "+quiet+": queued\n") {
		t.Fatalf("send: %q", out)
	}

	unknown := "w_00000000000000000000000000000009"
	for _, args := range [][]string{{"send", unknown, "hello"}, {"conversation", unknown}} {
		code, out, diag := invoke(t, root, args...)
		if code != 4 || out != "" || diag != "validation: workstream "+unknown+" is not in the active project; list workstreams with osmia status\n" {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}
	code, out, diag := invoke(t, root, "send", stream, "  ")
	if code != 4 || out != "" || diag != "validation: text must not be empty\n" {
		t.Fatalf("empty message: %d %q %q", code, out, diag)
	}
	for _, args := range [][]string{{"send", stream}, {"send", "not-a-workstream", "hello"}, {"send", stream, "a", "b"}, {"conversation"}, {"conversation", project}, {"conversation", stream, quiet}, {"send", stream, "hello", "--hard"}} {
		code, out, diag := invoke(t, root, args...)
		if code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}

	empty, _ := emptyFixture(t)
	idle, err := service.Start(context.Background(), empty)
	must(t, err)
	t.Cleanup(func() { idle.Close() })
	for _, args := range [][]string{{"send", stream, "hello"}, {"conversation", stream}} {
		code, out, diag := invoke(t, empty.Config.Root, args...)
		if code != 4 || out != "" || diag != "no_project: no project is configured; add one with osmia project add\n" {
			t.Fatalf("no project %v: %d %q %q", args, code, out, diag)
		}
	}
	s.Close()
	code, out, diag = invoke(t, root, "send", stream, "hello")
	if code != 3 || out != "" || !strings.Contains(diag, "cannot reach service") {
		t.Fatalf("stopped service: %d %q %q", code, out, diag)
	}
}

func TestConversationTextWithoutMessages(t *testing.T) {
	var w strings.Builder
	showConversation(&w, service.ConversationResponse{Workstream: stream, Entries: []service.ConversationEntry{}})
	if want := "Conversation: " + stream + "\n  no messages yet; send one with osmia send " + stream + " \"...\"\n"; w.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", w.String(), want)
	}
	w.Reset()
	showConversation(&w, service.ConversationResponse{Workstream: stream, Entries: []service.ConversationEntry{{Turn: "message_1", Kind: "message", Text: "Why?\n", At: written, State: service.TurnFailed}}})
	if want := "Conversation: " + stream + "\n2026-09-16T10:00:00Z owner [message_1 failed]\n  Why?\n"; w.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", w.String(), want)
	}
}
