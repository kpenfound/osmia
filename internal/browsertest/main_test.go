package browsertest

import (
	"flag"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// serial: sets process-wide test flags and closes the package's browsers after tests.
func TestMain(m *testing.M) {
	if !flag.Parsed() {
		flag.Parse()
	}
	fixturekit.ApplyServiceTestDefaults(flag.CommandLine)
	code := m.Run()
	Shutdown()
	os.Exit(code)
}
