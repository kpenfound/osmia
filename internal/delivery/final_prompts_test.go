package delivery

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestFinalReviewPromptsStateEverySectionAndNameNoSkill(t *testing.T) {
	t.Parallel()
	project := config.Project{Name: "dagger", Upstream: "dagger/dagger"}
	sections := []string{"## Purpose and setting", "## Tools and gates", "## Your part of the state machine", "## Output format", "## When you are done"}
	skills := []string{"handoff-reports", "design-sketch", "hill-climb-evaluator", "behavioral-testing", "simple-code", "no-deltas", "no-comments", "lint-discipline", "unslop", "typescript-best-practices", "debugging", "pr-review", "pr-writing", "skill"}
	for name, prompt := range map[string]string{"feature": FinalSystemPrompt(project), "climb": ClimbFinalSystemPrompt(project)} {
		for _, section := range sections {
			if !strings.Contains(prompt, section) {
				t.Errorf("%s final review prompt lacks %q", name, section)
			}
		}
		for _, skill := range skills {
			if strings.Contains(strings.ToLower(prompt), skill) {
				t.Errorf("%s final review prompt names the skill %q", name, skill)
			}
		}
		if !strings.Contains(prompt, "report") || !strings.Contains(prompt, FinalReportTool) {
			t.Errorf("%s final review prompt does not name %s", name, FinalReportTool)
		}
	}
}

func TestClimbFinalReviewKeepsGoAloneDecidesAndCarriesNoPlanAdvice(t *testing.T) {
	t.Parallel()
	prompt := ClimbFinalSystemPrompt(config.Project{Name: "dagger", Upstream: "dagger/dagger"})
	if !strings.Contains(prompt, "Go alone kept or discarded it by arithmetic") || !strings.Contains(prompt, "Go decides because a score is a measurement") {
		t.Errorf("the climb final review lost the rule that Go alone decides, or its reason:\n%s", prompt)
	}
	if planAdvice := regexp.MustCompile(`(?i)\b(units?|plans?|sizing)\b`).FindString(prompt); planAdvice != "" {
		t.Errorf("the climb final review carries unit or plan text %q:\n%s", planAdvice, prompt)
	}
}
