package service

import (
	"flag"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// serial: sets process-wide test flags and joins the shared browser after tests.
func TestMain(m *testing.M) {
	if !flag.Parsed() {
		flag.Parse()
	}
	fixturekit.ApplyServiceTestDefaults(flag.CommandLine)
	code := m.Run()
	browsertest.Shutdown()
	os.Exit(code)
}
