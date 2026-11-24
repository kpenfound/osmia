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
			Getenv:  func(name string) string { return map[string]string{"OPENROUTER_API_KEY": "key"}[name] },
		}
	}
	t.Cleanup(func() { doctorOptions = old })
}

func doctorRootWithConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("version = 1\n[profiles.default]\nagent = \"claude\"\nmodel = \"opus\"\n"), 0o600))
	return root
}

func TestDoctorReportsAMissingConfigurationFile(t *testing.T) {
	fakeDoctor(t)
	code, out, diag := invoke(t, t.TempDir(), "doctor")
	if code != 1 || diag != "" {
		t.Fatalf("code=%d stderr=%s", code, diag)
	}
	for _, want := range []string{"root\n", "  ✓ root directory", "  ✗ config.toml", "      → create ", "  ! jj", "checks: ", " 1 failed\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}

func TestDoctorJSONListsTheChecksOfAValidRoot(t *testing.T) {
	fakeDoctor(t)
	code, out, diag := invoke(t, doctorRootWithConfig(t), "doctor", "--json")
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
}

func TestDoctorFailsTheJevCheckWithoutItsAPIKey(t *testing.T) {
	fakeDoctor(t)
	old := doctorOptions
	t.Cleanup(func() { doctorOptions = old })
	doctorOptions = func(root config.Root) doctor.Options {
		o := old(root)
		o.Getenv = func(string) string { return "" }
		return o
	}
	code, out, diag := invoke(t, doctorRootWithConfig(t), "doctor")
	if code != 1 || diag != "" || !strings.Contains(out, "  ✗ jev") || !strings.Contains(out, "OPENROUTER_API_KEY is not set") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out, diag)
	}
}

func TestDoctorTakesNoArgumentsOrClientFlags(t *testing.T) {
	fakeDoctor(t)
	root := doctorRootWithConfig(t)
	for _, args := range [][]string{{"doctor", "extra"}, {"doctor", "--socket", "x.sock"}, {"doctor", "--detach"}} {
		if code, _, _ := invoke(t, root, args...); code != 2 {
			t.Fatalf("%v: code %d", args, code)
		}
	}
}

func TestServeFailureNamesDoctor(t *testing.T) {
	root := t.TempDir()
	code, _, diag := invoke(t, root, "serve")
	if code != 6 || !strings.Contains(diag, "osmia doctor") {
		t.Fatalf("code=%d stderr=%s", code, diag)
	}
}
