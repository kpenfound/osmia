package tailnet

import (
	"flag"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// serial: sets process-wide test flags.
func TestMain(m *testing.M) {
	if !flag.Parsed() {
		flag.Parse()
	}
	fixturekit.ApplyServiceTestDefaults(flag.CommandLine)
	os.Exit(m.Run())
}
