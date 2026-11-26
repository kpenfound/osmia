package intake

import (
	"github.com/kpenfound/osmia/internal/trace"
	"strings"
)

// OwnerActor is the provenance of messages the owner sends.
var OwnerActor = trace.Actor{Kind: "owner", ID: "local"}

// OwnerWritingGuidance tells the chief of staff the format of the text the
// owner's inbox shows. It belongs in the system prompt of every
// chief-of-staff turn.
const OwnerWritingGuidance = "## Inbox text\n" +
	"The owner reads your escalations, escalated contests and held units in an inbox, often on a phone, and decides from them in under a minute. Write each one in this order: " +
	"the decision you need, in one or two plain sentences; what is stuck, in one sentence; each option as one sentence that says what happens if the owner chooses it; and your recommendation, which is the option you would choose and its main reason. " +
	"Leave out how you got there, evidence that does not change the choice, turn names, file paths, record revisions and commit hashes. The owner can read those in the feed and the trace, and they hide the decision on a small screen. " +
	"Say each point once, because the fields are read together."

// Conversation returns a turn's message and, once the turn completed with a
// final response, that response.
func Conversation(q trace.QueuedTurn) []ConversationEntry {
	state := TurnQueued
	switch {
	case q.Claim == nil:
	case q.CompletedAt.IsZero():
		state = TurnRunning
	case q.Status() == "failed" || q.Status() == "interrupted":
		state = TurnFailed
	default:
		state = TurnDone
	}
	req := q.Request
	out := []ConversationEntry{{Turn: req.TurnID, Kind: "message", Text: req.Prompt, At: req.At, State: state, QueuedReason: QueuedReason(trace.Thread{Turns: []trace.QueuedTurn{q}}, q)}}
	if !q.CompletedAt.IsZero() && q.Response != nil && q.Response.Result.FinalResponse != "" {
		out = append(out, ConversationEntry{Turn: req.TurnID, Kind: "response", Text: q.Response.Result.FinalResponse, At: q.Response.At, State: state})
	}
	return out
}

// QueuedReason describes the earlier work on this thread without changing the
// turn's existing state. Durable sequence, rather than request time, orders it.
func QueuedReason(th trace.Thread, turn trace.QueuedTurn) string {
	if turn.Claim != nil || !turn.CompletedAt.IsZero() {
		return ""
	}
	for _, earlier := range th.Turns {
		if earlier.Request.TurnID == turn.Request.TurnID {
			break
		}
		if !earlier.CompletedAt.IsZero() {
			continue
		}
		kind := "background"
		switch {
		case earlier.Request.Actor == OwnerActor:
			kind = "owner message"
		case strings.Contains(earlier.Request.Cause, "event") || strings.HasPrefix(earlier.Request.TurnID, "event"):
			kind = "event"
		case strings.Contains(earlier.Request.Cause, "answer"):
			kind = "answer"
		case strings.Contains(earlier.Request.TurnID, "contest"):
			kind = "contest decision"
		case strings.Contains(earlier.Request.TurnID, "amend"):
			kind = "amendment"
		}
		article := "a "
		if kind == "event" || kind == "owner message" || kind == "answer" || kind == "amendment" {
			article = "an "
		}
		return "waiting for " + article + kind + " turn on this thread"
	}
	return "starting"
}
