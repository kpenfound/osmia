package lifecycle_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/lifecycle"
)

func TestForemanAndWorkgroupPlanningPromptsStateEverySectionAndNameNoSkill(t *testing.T) {
	t.Parallel()
	sections := []string{"## Purpose and setting", "## Tools and gates", "## Your part of the state machine", "## Output format", "## When you are done"}
	skills := []string{"handoff-reports", "design-sketch", "hill-climb-evaluator", "behavioral-testing", "simple-code", "no-deltas", "no-comments", "lint-discipline", "unslop", "typescript-best-practices", "debugging", "pr-review", "pr-writing", "skill"}
	for name, prompt := range map[string]string{
		"project spec":    lifecycle.ProjectSpecDraftSystemPrompt(),
		"delta architect": lifecycle.DeltaSystemPrompt("architect"),
		"delta committee": lifecycle.DeltaSystemPrompt("committee"),
	} {
		for _, section := range sections {
			if !strings.Contains(prompt, section) {
				t.Errorf("%s prompt lacks %q", name, section)
			}
		}
		for _, skill := range skills {
			if strings.Contains(strings.ToLower(prompt), skill) {
				t.Errorf("%s prompt names the skill %q", name, skill)
			}
		}
	}
	for _, tool := range []string{lifecycle.DeltaDraftTool, lifecycle.DeltaReviewTool, lifecycle.DeltaReplyTool} {
		if description := lifecycle.DeltaToolDescription(tool); !strings.Contains(description, "approves nothing") {
			t.Errorf("%s description %q does not say it approves nothing", tool, description)
		}
	}
}
