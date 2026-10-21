package intake

import (
	"github.com/kpenfound/osmia/internal/trace"
	"strings"
)

// ownerActor is the provenance of messages the owner sends.
var OwnerActor = trace.Actor{Kind: "owner", ID: "local"}

// ownerWritingGuidance tells the chief of staff how to write the text the
// owner's inbox shows. It belongs in the system prompt of every
// chief-of-staff turn.
const OwnerWritingGuidance = "The owner reads your escalations, escalated contests and units held for them in an inbox, often on a phone, and must be able to decide from it in under a minute. Write that text for them: " +
	"lead with the decision you need, in one or two plain sentences; say what is stuck in a sentence; give each option as one sentence saying what happens if they choose it; keep the recommendation to the option you would choose and its main reason. " +
	"Leave out how you got there, evidence that does not change the choice, turn names, file paths, record revisions and commit hashes; the owner can read those in the feed and the trace. " +
	"Prefer short sentences and plain words to jargon, and a short list to a long paragraph. Never repeat the same point in two fields."

// conversation returns a turn's message and, once the turn completed with a
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

// queuedReason describes the earlier work on this thread without changing the
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
