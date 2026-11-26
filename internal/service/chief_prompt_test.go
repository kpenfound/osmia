package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/questions"
)

func TestEveryChiefSystemPromptCarriesTheRolePromptAndNamesNoSkill(t *testing.T) {
	for name, prompt := range map[string]string{
		"owner message": fmt.Sprintf(chiefPrompt, "w_1") + "\n\n" + chiefGuidance(),
		"follow-up":     fmt.Sprintf(followUpPrompt, "w_1") + "\n\n" + chiefGuidance(),
		"events":        fmt.Sprintf(intake.ChiefEvents, "w_1") + "\n\n" + intake.ChiefRolePrompt + "\n\n" + questions.Guidance,
	} {
		if !strings.HasPrefix(prompt, "You are the chief of staff for workstream w_1.") || !strings.Contains(prompt, intake.ChiefRolePrompt) {
			t.Errorf("%s prompt does not open with the chief's role: %.200s", name, prompt)
		}
		lower := strings.ToLower(prompt)
		for _, skill := range []string{"handoff-reports", "design-sketch", "hill-climb-evaluator", "behavioral-testing", "simple-code", "no-deltas", "lint-discipline", "unslop", "typescript-best-practices", "pr-review", "no-comments", "pr-writing", "skill"} {
			if strings.Contains(lower, skill) {
				t.Errorf("%s prompt names %q", name, skill)
			}
		}
		for _, banned := range []string{"prefer short sentences", "plain words to jargon"} {
			if strings.Contains(lower, banned) {
				t.Errorf("%s prompt keeps the general prose preference %q", name, banned)
			}
		}
	}
}
