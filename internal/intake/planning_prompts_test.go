package intake

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/shed"
)

var planningProject = config.Project{Name: "dagger", Upstream: "dagger/dagger"}

var baseSections = []string{"## Purpose and setting", "## Tools and gates", "## Your part of the state machine", "## Output format", "## When you are done"}

func planningBasePrompts() map[string]string {
	return map[string]string{
		"architect":           ArchitectSystemPrompt(planningProject),
		"amendment architect": AmendmentSystemPrompt(planningProject),
		"climb architect":     ClimbSystemPrompt(planningProject),
		"committee":           CommitteeSystemPrompt(planningProject),
		"amendment committee": AmendmentCommitteeSystemPrompt(planningProject, RoundInput{Amendment: "a1"}),
		"climb committee":     ClimbCommitteeSystemPrompt(planningProject),
	}
}

func TestPlanningBasePromptsStateEverySectionWithNoSkillsConfigured(t *testing.T) {
	t.Parallel()
	for name, prompt := range planningBasePrompts() {
		for _, section := range baseSections {
			if !strings.Contains(prompt, section) {
				t.Errorf("%s prompt lacks %q:\n%s", name, section, prompt)
			}
		}
		for _, skill := range configuredSkillNames {
			if strings.Contains(strings.ToLower(prompt), skill) {
				t.Errorf("%s prompt names the skill %q", name, skill)
			}
		}
	}
}

func TestArchitectPromptsFinishTheWholeTaskAndNeverAskForReasoning(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"architect", "amendment architect", "climb architect"} {
		prompt := planningBasePrompts()[name]
		if !strings.Contains(prompt, "Finish the whole task") {
			t.Errorf("%s prompt carries no finish-the-whole-task guidance:\n%s", name, prompt)
		}
	}
	reasoning := regexp.MustCompile(`(?i)think (about|through|step)|step by step|your reasoning|explain (why|your)|write out`)
	task := map[string]string{
		"draft":        ArchitectPrompt("handed/stdin", 2, "problems", true, "note", nil),
		"climb draft":  ClimbPrompt("handed/stdin", 2, "problems", true, "note", nil),
		"reply":        ReplyPrompt(RoundInput{Round: 1}, shed.Pin{}, nil, nil),
		"redraft":      RedraftPrompt(RoundInput{Round: 1}, shed.Pin{}, nil, nil, "note"),
		"climb reply":  ClimbReplyPrompt(RoundInput{Round: 1}, shed.Pin{}, nil, nil),
		"amend system": AmendmentSystemPrompt(planningProject),
	}
	for name, prompt := range planningBasePrompts() {
		task["system "+name] = prompt
	}
	for name, prompt := range task {
		if reasoning.MatchString(prompt) {
			t.Errorf("%s asks the agent to write out its reasoning:\n%s", name, prompt)
		}
	}
}

func TestClimbPromptsKeepGoAloneDecidesAndCarryNoPlanAdvice(t *testing.T) {
	t.Parallel()
	planAdvice := regexp.MustCompile(`(?i)\bplans?\b|plan#|sizing|mason turn|\b(a|each|every|one|the) units?\b|spec\.md`)
	prompts := map[string]string{
		"architect system": ClimbSystemPrompt(planningProject),
		"draft":            ClimbPrompt("handed/stdin", 2, "problems", true, "note", []string{"answer"}),
		"committee system": ClimbCommitteeSystemPrompt(planningProject),
		"committee round":  ClimbCommitteePrompt(RoundInput{Round: 1}, nil, nil),
		"reply":            ClimbReplyPrompt(RoundInput{Round: 1}, shed.Pin{}, nil, nil),
	}
	for name, prompt := range prompts {
		if planAdvice.MatchString(prompt) {
			t.Errorf("climb %s prompt carries unit or plan text %q:\n%s", name, planAdvice.FindString(prompt), prompt)
		}
	}
	for _, name := range []string{"architect system", "committee system"} {
		prompt := prompts[name]
		if !strings.Contains(prompt, "Go alone decides whether an attempt is kept") || !strings.Contains(prompt, "because a score is a measurement") {
			t.Errorf("climb %s prompt lost the rule that Go alone decides, or its reason:\n%s", name, prompt)
		}
	}
}

func TestClimbCommitteeMembersGetNoPlanPerspective(t *testing.T) {
	t.Parallel()
	for name, focus := range CommitteePerspectives {
		if !strings.Contains(focus, "unit") && !strings.Contains(focus, "plan") {
			continue
		}
		if strings.Contains(ClimbCommitteeSystemPrompt(planningProject), focus) {
			t.Errorf("the climb committee prompt carries the %s perspective", name)
		}
	}
}
