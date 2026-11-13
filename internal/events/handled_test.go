package events

import (
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/trace"
)

// An event is handled once a turn that claimed it has ended in any status,
// and not while that turn is queued or running or before any turn claimed it.
func TestHandledFollowsTheTurnsThatClaimedTheEvent(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	entry := trace.OutboxEntry{History: []trace.DeliveryAction{{Kind: "claim", Token: "delivery_one"}}}
	turn := func(token string, response *trace.TurnResponse) trace.QueuedTurn {
		q := trace.QueuedTurn{Request: trace.TurnRequest{TurnID: TurnID(token)}, Response: response}
		if response != nil {
			q.CompletedAt = at
		}
		return q
	}
	for name, tc := range map[string]struct {
		entry trace.OutboxEntry
		turns []trace.QueuedTurn
		want  bool
	}{
		"no turn yet":          {entry, nil, false},
		"turn of another":      {entry, []trace.QueuedTurn{turn("delivery_other", &trace.TurnResponse{})}, false},
		"running":              {entry, []trace.QueuedTurn{turn("delivery_one", nil)}, false},
		"completed":            {entry, []trace.QueuedTurn{turn("delivery_one", &trace.TurnResponse{})}, true},
		"failed":               {entry, []trace.QueuedTurn{turn("delivery_one", &trace.TurnResponse{Failure: "model error"})}, true},
		"failed then retrying": {trace.OutboxEntry{History: []trace.DeliveryAction{{Kind: "claim", Token: "delivery_one"}, {Kind: "claim", Token: "delivery_two"}}}, []trace.QueuedTurn{turn("delivery_one", &trace.TurnResponse{Failure: "model error"}), turn("delivery_two", nil)}, true},
		"acknowledged":         {trace.OutboxEntry{Acknowledged: true}, nil, true},
	} {
		if got := Handled(tc.entry, trace.Thread{Turns: tc.turns}); got != tc.want {
			t.Errorf("%s: handled %v, want %v", name, got, tc.want)
		}
	}
}
