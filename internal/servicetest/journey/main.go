package journey

import (
	"flag"
	"testing"

	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// Main applies the service test defaults, runs the package's tests and shuts
// down the shared browser. A test package's TestMain returns its result to
// os.Exit.
func Main(m *testing.M) int {
	if !flag.Parsed() {
		flag.Parse()
	}
	fixturekit.ApplyServiceTestDefaults(flag.CommandLine)
	code := m.Run()
	browsertest.Shutdown()
	return code
}
