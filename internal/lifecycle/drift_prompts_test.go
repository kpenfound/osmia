package lifecycle_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
)

func TestDriftSystemPromptsStateEveryPartAndNameNoSkill(t *testing.T) {
	t.Parallel()
	mason := lifecycle.DriftMasonSystemPrompt(config.Project{Name: "demo", Upstream: "https://example.test/demo.git"})
	reviewer := lifecycle.DriftReviewerSystemPrompt()
	for name, text := range map[string]string{"drift mason": mason, "drift reviewer": reviewer} {
		for _, want := range []string{"## Your role", "## Your setting", "## Your tools and gates", "## Where you act in the state machine", "## When you are done", lifecycle.DriftScopeRule} {
			if !strings.Contains(text, want) {
				t.Errorf("%s prompt lacks %q", name, want)
			}
		}
		if strings.Contains(strings.ToLower(text), "skill") {
			t.Errorf("%s prompt names a skill", name)
		}
	}
	if !strings.Contains(mason, "You hold no version control tool") || !strings.Contains(mason, "The owner decides the spec") {
		t.Errorf("the drift mason lost its limits:\n%s", mason)
	}
}
