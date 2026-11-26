package thread

import (
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// LegacyBaseline recovers the system prompt of a session recorded before
// attempts carried it, from the turn that opened the session. It answers false
// when the opening turn's records do not show the whole system prompt the
// backend was given, in which case the turn replays into a new session.
type LegacyBaseline func(role string, opening trace.QueuedTurn) (string, bool)

func sessionBaseline(t trace.Thread, before uint64, session coreadapter.BackendSession, legacy LegacyBaseline) (string, bool) {
	for hops := 0; hops <= len(t.Turns); hops++ {
		q, a, ok := lastTurnOf(t, before, session)
		if !ok {
			return "", false
		}
		if a.SystemPrompt != nil {
			return *a.SystemPrompt, true
		}
		switch a.Path {
		case "replay":
			if legacy == nil {
				return "", false
			}
			prompt, ok := legacy(t.Identity.Role, q)
			return prompt, ok
		case "resume":
			before, session = q.Sequence, a.SourceSession
		default:
			return "", false
		}
	}
	return "", false
}

func lastTurnOf(t trace.Thread, before uint64, session coreadapter.BackendSession) (trace.QueuedTurn, trace.TurnAttempt, bool) {
	for i := len(t.Turns) - 1; i >= 0; i-- {
		q := t.Turns[i]
		if q.Sequence >= before || q.Response == nil || q.CompletedAt.IsZero() || len(q.Attempts) == 0 {
			continue
		}
		a := q.Attempts[len(q.Attempts)-1]
		if a.Result != nil && a.Result.Session == session && q.Response.Result.Session == session {
			return q, a, true
		}
	}
	return trace.QueuedTurn{}, trace.TurnAttempt{}, false
}
