package lifecycle_test

import (
	"strings"
	"testing"
)

func assertAcceptanceDraftInstructions(t *testing.T, prompt string) {
	t.Helper()
	for _, want := range []string{"Acceptance states observable behaviours", "tests the unit adds as evidence", "Never require a whole-suite check pass or run", "no need to list existing tests to keep passing", "service-owned check gate catches regressions", "factory runs and selects checks itself", "smart-check selection"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("draft instructions lack %q: %s", want, prompt)
		}
	}
}
