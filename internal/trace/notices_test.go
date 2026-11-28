package trace

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

func TestNotifyIsDurableIdempotentAndBoundToActiveManager(t *testing.T) {
	ctx := context.Background()
	r, root, p := create(t)
	_, err := r.EnsureManager(ctx, streamID, at, owner)
	if err != nil {
		t.Fatal(err)
	}
	h := header("turn-request", "request_notice")
	h.Actor = owner
	_, err = r.EnqueueTurn(ctx, TurnRequest{Header: h, AgentID: Manager, ThreadID: Manager, TurnID: "notice", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Notify the project"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.ClaimTurn(ctx, streamID, Manager, "claim", "/owned/notice", at)
	if err != nil {
		t.Fatal(err)
	}
	scope := coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Thread: Manager, Turn: "notice", Role: Manager}
	tool := r.NotifyTool(scope, func() time.Time { return at })
	input := json.RawMessage(`{"text":"Recheck the owner ruling before implementing."}`)
	first, err := tool.Handle(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := tool.Handle(ctx, input)
	if err != nil || string(first) != string(retry) {
		t.Fatalf("retry %s %v", retry, err)
	}
	var doc Document
	if err := json.Unmarshal(first, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Actor.Kind != "agent" || doc.Actor.ID != ManagerDisplay || doc.Workstream != "" || doc.Cause != "request_notice" {
		t.Fatalf("notice provenance: %+v", doc)
	}
	var shown ProjectNotice
	if err := json.Unmarshal([]byte(doc.Content), &shown); err != nil || shown.Scope.Role != ManagerDisplay || shown.Scope.Thread != ManagerDisplay {
		t.Fatalf("notice result names the manager's stored identity: %+v %v", shown.Scope, err)
	}
	other := scope
	other.Role = "mason"
	if _, err := r.NotifyTool(other, func() time.Time { return at }).Handle(ctx, input); err == nil {
		t.Fatal("mason published notice")
	}
	other = scope
	other.Turn = "missing"
	if _, err := r.NotifyTool(other, func() time.Time { return at }).Handle(ctx, input); err == nil {
		t.Fatal("unclaimed turn published notice")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	docs, err := Read[Document](reopened, "")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, d := range docs {
		if d.ID == doc.ID {
			count++
			var stored ProjectNotice
			if err := json.Unmarshal([]byte(d.Content), &stored); err != nil || d.Actor.ID != Manager || stored.Scope != scope {
				t.Fatalf("stored notice: %+v %+v %v", d.Actor, stored.Scope, err)
			}
		}
	}
	if count != 1 {
		t.Fatalf("notice count %d", count)
	}
	if _, err := reopened.NotifyTool(scope, func() time.Time { return at }).Handle(ctx, input); err == nil {
		t.Fatal("stale session published notice")
	}
}
