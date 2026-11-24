package executionjourney

import (
	testing "testing"

	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func assertCheckGateReviewInstructions(t *testing.T, prompt string) {
	t.Helper()
	intakefixture.AssertCheckGateReviewInstructions(t, prompt)
}
