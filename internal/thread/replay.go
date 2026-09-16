package thread

import (
	"encoding/json"
	"fmt"

	"github.com/kpenfound/osmia/internal/trace"
)

// ReplayLimits bounds the entire encoded context, including provenance and the
// omitted-prefix marker. Zero fields use defaults; negative fields are invalid.
type ReplayLimits struct{ MaxTurns, MaxBytes int }
type replayExchange struct {
	Sequence      uint64       `json:"sequence"`
	Request       trace.Header `json:"request"`
	Response      trace.Header `json:"response"`
	ThreadID      string       `json:"thread_id"`
	TurnID        string       `json:"turn_id"`
	Prompt        string       `json:"prompt"`
	FinalResponse string       `json:"final_response"`
}
type replayContext struct {
	Source    string           `json:"source"`
	Omitted   int              `json:"omitted_prefix_exchanges"`
	Exchanges []replayExchange `json:"exchanges"`
}

// Replay renders only completed owned request/final-response pairs before the
// claimed sequence. System prompts, supplied history and backend files are excluded.
func Replay(t trace.Thread, before uint64, limits ReplayLimits) (string, uint64, int, error) {
	if limits.MaxTurns == 0 {
		limits.MaxTurns = 20
	}
	if limits.MaxBytes == 0 {
		limits.MaxBytes = 64 * 1024
	}
	if limits.MaxTurns < 0 || limits.MaxBytes < 0 {
		return "", 0, 0, fmt.Errorf("invalid replay limits")
	}
	pairs := []replayExchange{}
	for _, q := range t.Turns {
		if q.Sequence >= before {
			break
		}
		if q.Response == nil || q.CompletedAt.IsZero() {
			continue
		}
		// Interrupted or failed attempts may contain only partial output.
		r := q.Response
		if r.Failure != "" || r.Result.IsError || r.Result.Cancelled || r.Result.TimedOut || r.Result.ExitCode != 0 || r.Result.Signal != 0 {
			continue
		}
		pairs = append(pairs, replayExchange{q.Sequence, q.Request.Header, r.Header, q.Request.ThreadID, q.Request.TurnID, q.Request.Prompt, r.Result.FinalResponse})
	}
	start := len(pairs)
	var encoded []byte
	for {
		data, err := json.Marshal(replayContext{Source: "osmia-owned-log", Omitted: start, Exchanges: pairs[start:]})
		if err != nil {
			return "", 0, 0, err
		}
		if len(data) > limits.MaxBytes {
			if encoded == nil {
				return "", 0, 0, fmt.Errorf("replay byte limit cannot fit omission marker")
			}
			start++
			break
		}
		encoded = data
		if start == 0 || len(pairs)-start == limits.MaxTurns {
			break
		}
		start--
	}
	var from uint64
	if start < len(pairs) {
		from = pairs[start].Sequence
	}
	return string(encoded), from, start, nil
}
