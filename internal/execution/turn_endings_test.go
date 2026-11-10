package execution

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func completedTurn(result coreadapter.SessionResult, failure string, class coreadapter.FailureKind) trace.QueuedTurn {
	return trace.QueuedTurn{Sequence: 1, CompletedAt: time.Unix(1, 0), Response: &trace.TurnResponse{Result: result, Failure: failure, FailureClass: class}}
}

func TestEndingsWithoutAReport(t *testing.T) {
	t.Parallel()
	done := &coreadapter.Outcome{Status: masonDone}
	for _, tc := range []struct {
		name   string
		turn   trace.QueuedTurn
		ended  bool
		taken  bool
		prompt string
	}{
		{"clean ending without done is classified", completedTurn(coreadapter.SessionResult{}, "", ""), false, false, ""},
		{"clean ending after a refused done", completedTurn(coreadapter.SessionResult{Refusal: "learning 1 is empty"}, "", ""), true, false, "learning 1 is empty"},
		{"behavioural failure", completedTurn(coreadapter.SessionResult{}, "the session failed: reported an outcome the role does not allow", coreadapter.Behavioural), true, false, "it failed: the session failed"},
		{"failure after a refused done", completedTurn(coreadapter.SessionResult{Refusal: "outcome is required"}, "boom", coreadapter.Behavioural), true, false, "outcome is required"},
		{"failure that used up its infrastructure retries", completedTurn(coreadapter.SessionResult{IsError: true}, "crashed", coreadapter.Infrastructure), false, false, ""},
		{"interrupted", completedTurn(coreadapter.SessionResult{Cancelled: true}, "", ""), false, false, ""},
		{"question", completedTurn(coreadapter.SessionResult{Outcome: &coreadapter.Outcome{Status: "waiting"}, Refusal: "x"}, "", ""), false, false, ""},
		{"accepted done", completedTurn(coreadapter.SessionResult{Outcome: done}, "", ""), false, true, ""},
		{"accepted done then a behavioural failure", completedTurn(coreadapter.SessionResult{Outcome: done, IsError: true}, "failed", coreadapter.Behavioural), false, true, ""},
		{"accepted done then an exhausted infrastructure failure", completedTurn(coreadapter.SessionResult{Outcome: done, IsError: true}, "failed", coreadapter.Infrastructure), false, false, ""},
		{"accepted done then an interruption", completedTurn(coreadapter.SessionResult{Outcome: done, Cancelled: true}, "", ""), false, false, ""},
		{"not completed", trace.QueuedTurn{Response: &trace.TurnResponse{Result: coreadapter.SessionResult{Refusal: "x"}}}, false, false, ""},
	} {
		reason, ended := endingWithoutReport(tc.turn)
		if ended != tc.ended || tc.taken != (tc.turn.Response != nil && !tc.turn.CompletedAt.IsZero() && reportTaken(tc.turn)) {
			t.Errorf("%s: ended %t (%q), taken %t", tc.name, ended, reason, reportTaken(tc.turn))
		}
		if !ended {
			continue
		}
		prompt := continuationPrompt(tc.turn, nil)
		if !strings.Contains(prompt, tc.prompt) || strings.Contains(prompt, "without calling an outcome tool") {
			t.Errorf("%s: prompt %q lacks %q", tc.name, prompt, tc.prompt)
		}
	}
}

func TestContinuationPromptForAReportWithoutACandidate(t *testing.T) {
	t.Parallel()
	turn := completedTurn(coreadapter.SessionResult{Outcome: &coreadapter.Outcome{Status: masonDone}, TimedOut: true}, "", coreadapter.Behavioural)
	prompt := continuationPrompt(turn, errors.New("git add: locked"))
	for _, want := range []string{"accepted the report", "it ran out of time", "git add: locked", "call done again"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt %q lacks %q", prompt, want)
		}
	}
}
