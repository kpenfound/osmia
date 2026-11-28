package trace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

func TestToolCallsRecordIntentCompletionAndInterruptedEffect(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	h := header("turn-request", "request_audit")
	_, err := r.EnqueueTurn(ctx, TurnRequest{Header: h, AgentID: Manager, ThreadID: Manager, TurnID: "audit", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Read"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.ClaimTurn(ctx, streamID, Manager, "claim", "/owned/audit", at)
	if err != nil {
		t.Fatal(err)
	}
	scope := coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Thread: Manager, Turn: "audit", Role: Manager}
	tool := coreadapter.Tool{Name: "file_read", Effect: coreadapter.ToolRead}
	complete, err := r.BeginTool(ctx, scope, tool, json.RawMessage(`{"path":"a.go"}`), func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	if err := complete(ctx, json.RawMessage(`{"text":"package a"}`), nil); err != nil {
		t.Fatal(err)
	}
	_, err = r.BeginTool(ctx, scope, tool, json.RawMessage(`{"path":"b.go"}`), func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	docs, err := Read[Document](r, streamID)
	if err != nil {
		t.Fatal(err)
	}
	latest := map[string]ToolCall{}
	for _, d := range docs {
		if !strings.HasPrefix(d.Path, "tools/") {
			continue
		}
		var call ToolCall
		if err := json.Unmarshal([]byte(d.Content), &call); err != nil {
			t.Fatal(err)
		}
		latest[d.ID] = call
		if d.Actor.ID != Manager || d.Cause != "request_audit" {
			t.Fatal("missing tool provenance")
		}
	}
	started, completed := 0, 0
	for _, call := range latest {
		if call.State == "started" {
			started++
		} else {
			completed++
		}
	}
	if len(latest) != 2 || started != 1 || completed != 1 {
		t.Fatalf("calls %+v", latest)
	}
	if _, err := r.BeginTool(ctx, scope, tool, json.RawMessage(`{}`), func() time.Time { return at }); err == nil {
		t.Fatal("stale turn audited")
	}
}
