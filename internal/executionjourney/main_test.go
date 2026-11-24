package executionjourney

import (
	"flag"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestMain(m *testing.M) {
	if os.Getenv("OSMIA_TEST_CRASH_ROOT") != "" {
		os.Exit(serviceStuckAtTheLandingStep())
	}
	if !flag.Parsed() {
		flag.Parse()
	}
	fixturekit.ApplyServiceTestDefaults(flag.CommandLine)
	code := m.Run()
	browsertest.Shutdown()
	os.Exit(code)
}
