package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSbx puts an sbx on PATH whose sandboxes report the given Claude Code
// version. "missing" has no saved template.
func fakeSbx(t *testing.T, claude string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"create) " + map[bool]string{true: "echo 'template osmia:latest not found' >&2; exit 1", false: "exit 0"}[claude == "missing"] + " ;;\n" +
		"exec) echo '" + claude + "' ;;\n" +
		"rm) exit 0 ;;\n" +
		"*) exit 2 ;;\n" +
		"esac\n"
	must(t, os.WriteFile(filepath.Join(dir, "sbx"), []byte(script), 0o755))
	t.Setenv("PATH", dir)
}

func TestSandboxCheckExitsZeroForAClaudeThatRunsFable(t *testing.T) {
	fakeSbx(t, "2.1.321 (Claude Code)")
	code, out, diag := validateRun(t, "sandbox", "check")
	if code != 0 || diag != "" || !strings.Contains(out, "2.1.321") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
}

func TestSandboxCheckReportsItsReasonOnStderr(t *testing.T) {
	for name, tt := range map[string]struct{ claude, reason string }{
		"older":    {"2.1.320 (Claude Code)", "2.1.320"},
		"missing":  {"missing", "osmia:latest"},
		"unusable": {"nothing to read", "could not read"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeSbx(t, tt.claude)
			code, out, diag := validateRun(t, "sandbox", "check")
			if code != 1 || out != "" || !strings.HasPrefix(diag, "osmia sandbox check: ") || !strings.Contains(diag, tt.reason) || strings.Count(diag, "\n") != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
			}
		})
	}
}

func TestSandboxCommandsNameAMissingTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for args, want := range map[string]string{"check": "sbx is required on PATH", "build": "sbx is required on PATH"} {
		code, out, diag := validateRun(t, "sandbox", args)
		if code != 1 || out != "" || !strings.Contains(diag, want) {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q", args, code, out, diag)
		}
	}
	fakeSbx(t, "2.1.321")
	code, _, diag := validateRun(t, "sandbox", "build")
	if code != 1 || !strings.Contains(diag, "dagger is required on PATH") {
		t.Fatalf("code=%d stderr=%q", code, diag)
	}
}

func TestSandboxCommandsNeedNoRootConfigOrService(t *testing.T) {
	fakeSbx(t, "2.1.321")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	if code, _, diag := validateRun(t, "sandbox", "check"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, diag)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("the command created %v", entries)
	}
}

func TestSandboxRefusesInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"sandbox"},
		{"sandbox", "save"},
		{"sandbox", "build", "extra"},
		{"sandbox", "check", "--json"},
		{"sandbox", "check", "--root", "x"},
		{"sandbox", "build", "--socket", "x.sock"},
		{"sandbox", "build", "--config", "x.toml"},
	} {
		if code, out, diag := validateRun(t, args...); code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, out, diag)
		}
	}
}

func TestUsageListsTheSandboxCommands(t *testing.T) {
	code, out, _ := validateRun(t, "--help")
	for _, want := range []string{"sandbox build", "sandbox check", "osmia:latest", "2.1.321"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("usage does not mention %q: %q", want, out)
		}
	}
}
