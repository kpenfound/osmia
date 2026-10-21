package executionjourney

import (
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	testing "testing"
)

func assertCheckGateReviewInstructions(t *testing.T, prompt string) {
	t.Helper()
	intakefixture.AssertCheckGateReviewInstructions(t, prompt)
}
