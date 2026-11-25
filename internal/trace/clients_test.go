package trace

import (
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

func finished(sequence uint64, client, session string) QueuedTurn {
	result := coreadapter.SessionResult{Client: client, Session: coreadapter.BackendSession{Backend: "claude", ID: session}}
	return QueuedTurn{Sequence: sequence, Response: &TurnResponse{Result: result}, CompletedAt: time.Unix(int64(sequence), 0)}
}

func TestTheClientThatHoldsAThreadsSessionIsTheOneThatRanItsLatestSessionTurn(t *testing.T) {
	if client, ok := (Thread{}).SessionClient(); ok || client != "" {
		t.Fatalf("a thread with no turns holds a session on %q", client)
	}
	thread := Thread{Turns: []QueuedTurn{finished(1, "", "s1")}}
	if client, ok := thread.SessionClient(); !ok || client != coreadapter.LocalClient {
		t.Fatalf("a turn recorded without a client ran on the local client: %q %v", client, ok)
	}
	thread.Turns = append(thread.Turns, finished(2, "node-a", "s2"))
	if client, ok := thread.SessionClient(); !ok || client != "node-a" {
		t.Fatalf("the latest session's client: %q %v", client, ok)
	}
	noSession := finished(3, "node-b", "")
	noSession.Response.Result.Session = coreadapter.BackendSession{}
	running := QueuedTurn{Sequence: 4, Response: &TurnResponse{Result: coreadapter.SessionResult{Client: "node-c", Session: coreadapter.BackendSession{Backend: "claude", ID: "s4"}}}}
	thread.Turns = append(thread.Turns, noSession, running)
	if client, ok := thread.SessionClient(); !ok || client != "node-a" {
		t.Fatalf("a turn with no session or no completion moved the session: %q %v", client, ok)
	}
}
