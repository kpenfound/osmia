package browsertest

import (
	"flag"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestMain(m *testing.M) {
	if !flag.Parsed() {
		flag.Parse()
	}
	fixturekit.ApplyServiceTestDefaults(flag.CommandLine)
	code := m.Run()
	Shutdown()
	os.Exit(code)
}
