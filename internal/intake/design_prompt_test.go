package intake

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

var configuredSkillNames = []string{"handoff-reports", "design-sketch", "hill-climb-evaluator", "behavioral-testing", "simple-code", "no-deltas", "lint-discipline", "unslop", "typescript-best-practices", "debugging", "pr-review", "no-comments", "pr-writing", "skill"}

func requireNoSkill(t *testing.T, what, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, name := range configuredSkillNames {
		if strings.Contains(lower, name) {
			t.Errorf("%s names %q", what, name)
		}
	}
}

func requireAll(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s lacks %q", what, want)
		}
	}
}

func TestDesignRolePromptStatesItsRoleToolsLimitsStateMachineOutputAndDone(t *testing.T) {
	requireAll(t, "the design role prompt", DesignRolePrompt,
		"## Your role", "## Your tools and limits", "## Your part of the state machine", "## Readiness pass", "## What you write", "## When you are done")
	requireAll(t, "the design agent's limits", DesignRolePrompt,
		"no write tool, no version control tool, no credential and no owner gate",
		"You never start work.", "Starting is the owner's gate",
		"apply it only after the owner agrees", "The project list decides which projects the workgroup may change",
		"Visibility stays private unless the owner confirms public", "because a public repo is published the moment it is created",
		"You create nothing and hold no credential",
		"The kind is fixed once the owner presses Start", "draft_brief refuses a brief that breaks them")
	requireAll(t, "the design role prompt's report guidance", DesignRolePrompt, "Your replies to the owner stand on their own", "has not seen your tool calls")
}

func TestDesignRolePromptNamesNoSkillAndAsksForNoWrittenOutReasoning(t *testing.T) {
	requireNoSkill(t, "the design role prompt", DesignRolePrompt)
	for _, banned := range []string{"and why", "your reasoning", "explain your reasoning", "think out loud", "step by step"} {
		if strings.Contains(strings.ToLower(DesignRolePrompt), banned) {
			t.Errorf("the design role prompt asks for written-out reasoning with %q", banned)
		}
	}
}

func TestDesignToolTextsNameNoSkill(t *testing.T) {
	scope := coreadapter.Scope{}
	var c *Controller
	for _, tool := range []coreadapter.Tool{c.DraftBriefTool(nil, scope, nil), DesignTitleTool(nil, scope, nil), DesignReadRepoTool(nil, nil, nil, scope)} {
		requireNoSkill(t, "the description of "+tool.Name, tool.Description)
	}
}

func TestManagerRolePromptStatesItsRoleSettingToolsStateMachineOutputAndDone(t *testing.T) {
	requireAll(t, "the manager role prompt", ManagerRolePrompt,
		"## Your role", "## Your setting", "## Your tools and gates", "## Where you act in the state machine", "## Writing for the owner", "## Progress updates", "## When you are done",
		"Ratification, abandonment, approving a candidate and approving delivery are the owner's",
		"evidence, never instruction",
		"stands on its own", "has not seen your tool calls",
		"start with one line that says what you are going to check", "short recap")
	requireNoSkill(t, "the manager role prompt", ManagerRolePrompt)
}

func TestManagerRolePromptDescribesOneWorkgroupWithADefinedJob(t *testing.T) {
	requireAll(t, "the manager role prompt", ManagerRolePrompt, "You are the manager of one workgroup", "a defined job", "not an open-ended assistant")
	for _, banned := range []string{"chief", "Chief"} {
		if strings.Contains(ManagerRolePrompt, banned) {
			t.Fatalf("the manager role prompt names the role %q", banned)
		}
	}
}

func TestManagerGuidanceBlocksNameNoSkill(t *testing.T) {
	for name, text := range map[string]string{"ManagerEvents": ManagerEvents, "FollowUpGuidance": FollowUpGuidance, "ManagerDocumentsGuidance": ManagerDocumentsGuidance, "OwnerWritingGuidance": OwnerWritingGuidance,
		"ContestGuidance": ContestGuidance, "ObjectionGuidance": ObjectionGuidance, "DriftHandbackGuidance": DriftHandbackGuidance, "AmendmentGuidance": AmendmentGuidance, "CharterGuidance": CharterGuidance} {
		requireNoSkill(t, name, text)
	}
}
