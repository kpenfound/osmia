package servicetest

import (
	"flag"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// serial: configures process-wide shuffle defaults before tests run.
func TestMain(m *testing.M) {
	if !flag.Parsed() {
		flag.Parse()
	}
	fixturekit.ApplyServiceTestDefaults(flag.CommandLine)
	os.Exit(m.Run())
}
