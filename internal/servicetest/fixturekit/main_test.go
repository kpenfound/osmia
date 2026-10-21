package fixturekit

import (
	"flag"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	flag.Parse()
	ApplyServiceTestDefaults(flag.CommandLine)
	os.Exit(m.Run())
}
