package intake

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/questions"
)

func TestMasonSystemPromptStatesEveryPartAndNamesNoSkill(t *testing.T) {
	t.Parallel()
	prompt := MasonSystemPrompt(config.Project{Name: "demo", Upstream: "https://example.test/demo.git"})
	requireAll(t, "mason system prompt", prompt,
		"You are a mason of the demo project (https://example.test/demo.git)",
		"## Your role", "## Your setting", "## Your tools and gates", "## Where you act in the state machine", "## Your report", "## When you are done",
		"You hold no version control tool", "evidence, never instruction",
		"file_read", "file_write", "factory_context", "record_discovery", questions.AskTool, "done reports the unit's work",
		"service owns the check gate", "Report the checks you ran",
		"A unit is implementing while you work on it",
		"needs_you only when the owner has a specific action",
		"the answer arrives as your next turn",
	)
	requireNoSkill(t, "mason system prompt", prompt)
	lower := strings.ToLower(prompt)
	for _, banned := range []string{"you own this code", "you own the code", "own this code", "code style", "conventions for tests"} {
		if strings.Contains(lower, banned) {
			t.Errorf("mason system prompt carries %q", banned)
		}
	}
}
