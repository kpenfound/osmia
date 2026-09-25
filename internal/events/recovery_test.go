package events

import (
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestRecoveryContinuationKeepsEventPendingUntilChiefFinishes(t *testing.T) {
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	first := trace.QueuedTurn{Request: trace.TurnRequest{Header: trace.Header{ID: "request_event"}, TurnID: "events_one"},
		Response: &trace.TurnResponse{Header: trace.Header{ID: "response_event"}, Result: coreadapter.SessionResult{Cancelled: true}}, CompletedAt: at}
	second := trace.QueuedTurn{Request: trace.TurnRequest{Header: trace.Header{ID: "request_recover", Cause: "response_event", Actor: trace.Actor{Kind: "service", ID: "thread-recovery"}}, TurnID: "chief-recover-1"}}
	thread := trace.Thread{Turns: []trace.QueuedTurn{first, second}}
	if got := turnStates(thread)["events_one"]; got != turnPending {
		t.Fatalf("event with queued recovery: %v", got)
	}
	thread.Turns[1].Response = &trace.TurnResponse{Header: trace.Header{ID: "response_recover"}}
	thread.Turns[1].CompletedAt = at
	if got := turnStates(thread)["events_one"]; got != turnDone {
		t.Fatalf("event after recovery: %v", got)
	}
}
