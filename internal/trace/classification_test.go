package trace

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

func TestClassifyMasonTurn(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"Could you clarify which store to use?", "asked_in_prose"},
		{"I have completed the unit and the tests pass.", "claims_done"},
		{"I cannot complete this work.", "gave_up"},
		{"I edited the file and ran a check.", "unclear"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			result := coreadapter.SessionResult{FinalResponse: tc.text, ToolCounts: map[string]int{"file_write": 2}}
			got := ClassifyMasonTurn(result, "")
			if got == nil || got.Class != tc.want || got.ToolCounts["file_write"] != 2 || got.Window != tc.text || got.Evidence == "" {
				t.Fatalf("classification: %+v", got)
			}
		})
	}
}

func TestClassifyMasonTurnWindowAndExclusions(t *testing.T) {
	result := coreadapter.SessionResult{FinalResponse: "I cannot proceed. " + strings.Repeat("x", 2001) + "Could you clarify?"}
	got := ClassifyMasonTurn(result, "")
	if got.Class != "asked_in_prose" || len([]rune(got.Window)) != 2000 || strings.Contains(got.Window, "cannot proceed") {
		t.Fatalf("window: %+v", got)
	}
	for _, change := range []func(*coreadapter.SessionResult){
		func(r *coreadapter.SessionResult) { r.Outcome = &coreadapter.Outcome{Status: "done"} },
		func(r *coreadapter.SessionResult) { r.Outcome = &coreadapter.Outcome{Status: "waiting"} },
		func(r *coreadapter.SessionResult) { r.IsError = true },
		func(r *coreadapter.SessionResult) { r.Cancelled = true },
	} {
		copy := result
		change(&copy)
		if got := ClassifyMasonTurn(copy, ""); got != nil {
			t.Fatalf("classified excluded turn: %+v", got)
		}
	}
	if got := ClassifyMasonTurn(result, "transport error"); got != nil {
		t.Fatalf("classified failed turn: %+v", got)
	}
}
