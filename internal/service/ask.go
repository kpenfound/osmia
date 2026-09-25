package service

import (
	"strings"

	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
)

// askChain maps every turn of one operation on the thread to the attempt it
// continues: an attempt turn, named with prefix, to itself, and the turn that
// delivers the answer to a question one of them asked, and the turn that
// continues one of them after a hard pause stopped it, to that attempt.
func askChain(t trace.Thread, prefix string, asked []trace.QuestionState) map[string]string {
	origins := map[string]string{}
	for _, q := range t.Turns {
		if strings.HasPrefix(q.Request.TurnID, prefix) && !isContinuation(q.Request.TurnID) {
			origins[q.Request.TurnID] = q.Request.TurnID
		}
	}
	// Answers and continuations follow each other in any order, so both are
	// followed until neither adds a turn.
	for added := true; added; {
		added = false
		for _, q := range asked {
			answer := questions.TurnID(q.Asked.ID)
			if origin, ok := origins[q.Asked.Turn]; ok && q.Asked.Thread == t.Identity.ThreadID && origins[answer] == "" {
				origins[answer], added = origin, true
			}
		}
		for _, q := range t.Turns {
			id := q.Request.TurnID
			if origin, ok := origins[continuedTurn(id)]; ok && isContinuation(id) && origins[id] == "" {
				origins[id], added = origin, true
			}
		}
	}
	return origins
}

// chainTurns returns the thread's turns the chain maps, in thread order.
func chainTurns(t trace.Thread, origins map[string]string) []trace.QueuedTurn {
	var out []trace.QueuedTurn
	for _, q := range t.Turns {
		if _, ok := origins[q.Request.TurnID]; ok {
			out = append(out, q)
		}
	}
	return out
}

// attempts returns one turn per attempt of the chain, in order: the last
// turn of the attempt, which says where the attempt got to, named after the
// attempt, whose directory holds what the attempt delivered.
func attempts(chain []trace.QueuedTurn, origins map[string]string) []trace.QueuedTurn {
	var out []trace.QueuedTurn
	for _, q := range chain {
		origin := origins[q.Request.TurnID]
		if n := len(out); n > 0 && out[n-1].Request.TurnID == origin {
			out = out[:n-1]
		}
		q.Request.TurnID = origin
		out = append(out, q)
	}
	return out
}

// askedBy returns the ID of the question the thread's turn asked, or nothing.
func askedBy(asked []trace.QuestionState, thread, turn string) string {
	for _, q := range asked {
		if q.Asked.Thread == thread && q.Asked.Turn == turn {
			return q.Asked.ID
		}
	}
	return ""
}

// received returns the answers delivered to the questions the chain's turns
// asked on the thread, as their answer turns put them, for the prompt of an
// attempt that follows them.
func received(t trace.Thread, origins map[string]string, asked []trace.QuestionState) []string {
	var out []string
	for _, q := range asked {
		if _, ok := origins[q.Asked.Turn]; ok && q.Asked.Thread == t.Identity.ThreadID && q.State == trace.QuestionAnswered && q.Ruling != nil {
			out = append(out, questions.Prompt(q.Asked, *q.Ruling))
		}
	}
	return out
}

// answered is the prompt block that repeats the answers an attempt's
// predecessors received in the round, draft, reply or redraft named by what;
// empty without answers.
func answered(what string, answers []string) string {
	if len(answers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nThe answers to the questions you asked in this " + what + ":\n")
	for _, a := range answers {
		b.WriteString("\n" + a)
	}
	return b.String()
}
