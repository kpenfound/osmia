package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/doctor"
)

// fakeDoctor checks roots against a host whose every tool answers.
func fakeDoctor(t *testing.T) {
	t.Helper()
	old := doctorOptions
	doctorOptions = func(root config.Root) doctor.Options {
		return doctor.Options{
			Root:     root,
			LookPath: func(name string) (string, error) { return "/bin/" + name, nil },
			Command: func(_ context.Context, path string, _ ...string) (string, error) {
				return filepath.Base(path) + " 1.0.0", nil
			},
			CheckJJ: func(context.Context) (string, error) { return "", exec.ErrNotFound },
		}
	}
	t.Cleanup(func() { doctorOptions = old })
}

func TestDoctor(t *testing.T) {
	fakeDoctor(t)
	root := t.TempDir()
	code, out, diag := invoke(t, root, "doctor")
	if code != 1 || diag != "" {
		t.Fatalf("code=%d stderr=%s", code, diag)
	}
	for _, want := range []string{"root\n", "  ✓ root directory", "  ✗ config.toml", "      → create ", "  ! jj", "checks: ", " 1 failed\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}

	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("version = 1\n[profiles.default]\nagent = \"claude\"\nmodel = \"opus\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, diag = invoke(t, root, "doctor", "--json")
	if code != 0 || diag != "" {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out, diag)
	}
	var checks []doctor.Check
	if err := json.Unmarshal([]byte(out), &checks); err != nil {
		t.Fatal(err)
	}
	if len(checks) == 0 || checks[0].Group != doctor.GroupRoot {
		t.Fatalf("%+v", checks)
	}

	for _, args := range [][]string{{"doctor", "extra"}, {"doctor", "--socket", "x.sock"}, {"doctor", "--detach"}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: code %d", args, code)
		}
	}
}

// TestServeFailureNamesDoctor shows a startup failure pointing at doctor.
func TestServeFailureNamesDoctor(t *testing.T) {
	root := t.TempDir()
	code, _, diag := invoke(t, root, "serve")
	if code != 6 || !strings.Contains(diag, "osmia doctor") {
		t.Fatalf("code=%d stderr=%s", code, diag)
	}
}
