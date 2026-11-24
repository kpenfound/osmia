package executionjourney

import (
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/journey"
)

func TestMain(m *testing.M) {
	if os.Getenv("OSMIA_TEST_CRASH_ROOT") != "" {
		os.Exit(serviceStuckAtTheLandingStep())
	}
	journey.FreeRouting = true
	os.Exit(journey.Main(m))
}
