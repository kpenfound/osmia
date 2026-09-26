package osmia

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The dagger test containers pin a jj release and set OSMIA_REQUIRE_JJ, so a
// missing binary there fails; elsewhere the test skips.
func TestJJAvailable(t *testing.T) {
	path, err := exec.LookPath("jj")
	if err != nil {
		if os.Getenv("OSMIA_REQUIRE_JJ") != "" {
			t.Fatalf("jj is required but not on PATH: %v", err)
		}
		t.Skip("jj is not installed")
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Fatalf("jj --version: %v", err)
	}
	if !strings.HasPrefix(string(out), "jj ") {
		t.Fatalf("unexpected jj --version output %q", out)
	}
}
