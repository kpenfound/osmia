package trace

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

func TestNotesWritePreconditionAcrossTurns(t *testing.T) {
	ctx := context.Background()
	r, _, _ := create(t)
	if err := r.CreateThread(ctx, threadAgent()); err != nil {
		t.Fatal(err)
	}
	secondStream := config.WorkstreamID("w_00000000000000000000000000000002")
	if err := r.CreateWorkstream(ctx, secondStream, at, owner); err != nil {
		t.Fatal(err)
	}
	secondAgent := Agent{Header: header("agent", "mason2"), Role: "mason", ThreadID: "thread2"}
	secondAgent.Workstream = secondStream
	if err := r.CreateThread(ctx, secondAgent); err != nil {
		t.Fatal(err)
	}
	firstRequest := enqueue(t, r, "first")
	secondRequest := threadRequest("second")
	secondRequest.Header = header("turn-request", "request_second")
	secondRequest.Header.Workstream = secondStream
	secondRequest.AgentID, secondRequest.ThreadID = "mason2", "thread2"
	if _, err := r.EnqueueTurn(ctx, secondRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTurn(ctx, streamID, "mason", "claim1", "/one", at); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTurn(ctx, secondStream, "mason2", "claim2", "/two", at); err != nil {
		t.Fatal(err)
	}
	firstTools, err := r.NotesTools("mason", coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Thread: firstRequest.Request.ThreadID, Turn: firstRequest.Request.TurnID, Role: "mason"})
	if err != nil {
		t.Fatal(err)
	}
	secondTools, err := r.NotesTools("mason2", coreadapter.Scope{Project: string(projectID), Workstream: string(secondStream), Thread: secondRequest.ThreadID, Turn: secondRequest.TurnID, Role: "mason"})
	if err != nil {
		t.Fatal(err)
	}
	read := func(tools []coreadapter.Tool) struct {
		Text   string `json:"text"`
		SHA256 string `json:"sha256"`
	} {
		t.Helper()
		data, err := tools[0].Handle(ctx, json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Text   string `json:"text"`
			SHA256 string `json:"sha256"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	write := func(tools []coreadapter.Tool, text, expected string) struct {
		Written bool   `json:"written"`
		Reason  string `json:"reason"`
		SHA256  string `json:"sha256"`
	} {
		t.Helper()
		input, err := json.Marshal(map[string]string{"text": text, "expected_sha256": expected})
		if err != nil {
			t.Fatal(err)
		}
		data, err := tools[1].Handle(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Written bool   `json:"written"`
			Reason  string `json:"reason"`
			SHA256  string `json:"sha256"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	firstRead, secondRead := read(firstTools), read(secondTools)
	if firstRead.Text != "" || firstRead.SHA256 != missingNotesSHA256 || secondRead.SHA256 != firstRead.SHA256 {
		t.Fatalf("initial reads: %#v %#v", firstRead, secondRead)
	}
	if got := write(secondTools, "must not publish", "incorrect"); got.Written || got.SHA256 != missingNotesSHA256 || got.Reason == "" {
		t.Fatalf("bad missing precondition: %#v", got)
	}
	if got := read(firstTools); got.Text != "" || got.SHA256 != missingNotesSHA256 {
		t.Fatalf("refusal changed missing notes: %#v", got)
	}
	if got := write(firstTools, "first turn", firstRead.SHA256); !got.Written {
		t.Fatalf("first write: %#v", got)
	}
	stale := write(secondTools, "second turn", secondRead.SHA256)
	current := read(secondTools)
	if stale.Written || stale.SHA256 != current.SHA256 || stale.Reason == "" || current.Text != "first turn" {
		t.Fatalf("stale write: result=%#v current=%#v", stale, current)
	}
	if got := write(secondTools, "merged notes", current.SHA256); !got.Written {
		t.Fatalf("retry with current hash: %#v", got)
	}
	if got := read(firstTools); got.Text != "merged notes" {
		t.Fatalf("final notes: %#v", got)
	}
}
