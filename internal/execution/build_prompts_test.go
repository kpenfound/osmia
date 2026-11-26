package execution

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/trace"
)

var skillWords = []string{"skill", "handoff-reports", "design-sketch", "hill-climb-evaluator", "behavioral-testing", "simple-code", "no-deltas", "lint-discipline", "unslop", "typescript-best-practices", "pr-review", "no-comments", "pr-writing"}

func requireNoSkillWords(t *testing.T, what, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, word := range skillWords {
		if strings.Contains(lower, word) {
			t.Errorf("%s names %q", what, word)
		}
	}
}

func TestClimbMasonSystemPromptKeepsTheRuleThatGoAloneDecides(t *testing.T) {
	t.Parallel()
	prompt := fmt.Sprintf(climbMasonSystemPrompt, "demo", "https://example.test/demo.git")
	requireContains(t, "climb mason system prompt", prompt, []string{
		"## Your role", "## Your setting", "## Who decides", "## Your tools and gates", "## Where you act in the state machine", "## Your report", "## When you are done",
		"Go alone decides whether an attempt is kept, by arithmetic over the service's own measurement",
		"You never judge whether a score improved", "your own measurements, which you may run to learn, never count",
		"You hold no version control tool", "The protected paths are frozen",
		"Call done with the hypothesis you tested, the change you made and the effect you expect",
	})
	requireNoSkillWords(t, "climb mason system prompt", prompt)
}

func TestClimbReviewerSystemPromptKeepsTheRuleThatGoAloneDecides(t *testing.T) {
	t.Parallel()
	requireContains(t, "climb reviewer system prompt", climbReviewerSystemPrompt, []string{
		"## Your role", "## Your setting", "## What you read for", "## Your tools and gates", "## The check gate", "## Where you act in the state machine", "## Your verdict", "## When you are done",
		"Go kept it", "You decide only whether the candidate may land as the new best",
		"Go alone decides that by arithmetic", "Metric gaming", "Scope: a change the attempt's hypothesis does not explain",
		"Test passing is decided by the service-owned check gate",
		"this prompt wins",
	})
	requireNoSkillWords(t, "climb reviewer system prompt", climbReviewerSystemPrompt)
}

func TestAttemptContinuationNamesTheAttemptAndItsReport(t *testing.T) {
	t.Parallel()
	last := trace.QueuedTurn{Response: &trace.TurnResponse{Failure: "boom"}}
	got := attemptContinuationPrompt(last)
	requireContains(t, "attempt continuation", got, []string{"Continue the attempt from there and call done with your hypothesis, the change you made and the effect you expect"})
	if strings.Contains(got, "unit") {
		t.Errorf("attempt continuation names a unit: %s", got)
	}
	got = continuationPrompt(last, fmt.Errorf("no workspace"))
	requireContains(t, "unit continuation", got, []string{"You reported this unit done", "the unit's candidate", "check the unit's acceptance, and call done again."})
}

func TestMasonAndReviewerTaskTextNamesNoSkillAndNoTestStylePreference(t *testing.T) {
	t.Parallel()
	text := unitReviewPromptLead + unitReviewMateriality + unitReviewerSystemPrompt
	requireNoSkillWords(t, "review text", text)
	if strings.Contains(strings.ToLower(text), "conventions for tests") {
		t.Errorf("review text carries a test-style preference")
	}
}
