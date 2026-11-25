package climb

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProtectedPathsIncludeTheDefaultsAndTheSpecsAdditions(t *testing.T) {
	t.Parallel()
	got, err := ProtectedPaths(".dagger/modules/osmia-dev/", []string{"testdata/workload"}, []string{"./scripts/oracle.sh", "testdata/workload"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".dagger/modules/osmia-dev", "dagger.lock", "dagger.toml", "scripts/oracle.sh", "testdata/workload"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
	bare, err := ProtectedPaths("", nil, nil)
	if err != nil || !slices.Equal(bare, []string{"dagger.lock", "dagger.toml"}) {
		t.Fatalf("%q, %v", bare, err)
	}
	for _, bad := range []string{"", "..", "../x", "/etc", ".", "a/../../x"} {
		if _, err := ProtectedPaths("", nil, []string{bad}); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
}

func TestGateRefusesADiffThatTouchesAProtectedPath(t *testing.T) {
	t.Parallel()
	protected, err := ProtectedPaths(".dagger/modules/osmia-dev", []string{"testdata/workload"}, []string{"scripts/oracle.sh"})
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string][]string{
		"dagger.toml":                  {"dagger.toml"},
		"dagger.lock":                  {"internal/x.go", "dagger.lock"},
		"inside the dev module":        {".dagger/modules/osmia-dev/main.dang"},
		"a new file in the dev module": {".dagger/modules/osmia-dev/cheat/new.go"},
		"a declared fixture":           {"testdata/workload/big.json"},
		"a path the spec added":        {"scripts/oracle.sh"},
		"a leading ./":                 {"./dagger.toml"},
		"both sides of a rename":       {"internal/a.go", "testdata/workload/in.json"},
	}
	for name, changed := range refused {
		err := Gate(protected, changed)
		var refusal *ProtectedError
		if !errors.As(err, &refusal) || len(refusal.Touched) == 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	var refusal *ProtectedError
	err = Gate(protected, []string{"dagger.toml", "internal/x.go", ".dagger/modules/osmia-dev/main.dang", "dagger.toml"})
	if !errors.As(err, &refusal) || !slices.Equal(refusal.Touched, []string{".dagger/modules/osmia-dev/main.dang", "dagger.toml"}) || !strings.Contains(err.Error(), "dagger.toml") {
		t.Fatalf("%v", err)
	}
	for name, changed := range map[string][]string{
		"nothing changed":                nil,
		"the implementation":             {"internal/x.go", "cmd/osmia/main.go"},
		"a sibling with the same prefix": {"dagger.toml.bak", ".dagger/modules/osmia-dev-other/x", "testdata/workload2/x", "scripts/oracle.sh.orig"},
		"a different module":             {".dagger/modules/other/main.go"},
	} {
		if err := Gate(protected, changed); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDevModuleSourceIsTheMainModule(t *testing.T) {
	t.Parallel()
	source, err := DevModuleSource([]byte(`
[modules.go]
source = "dagger.io/go"

[modules.osmia-dev]
source = ".dagger/modules/osmia-dev"
main = true
`))
	if err != nil || source != ".dagger/modules/osmia-dev" {
		t.Fatalf("%q, %v", source, err)
	}
	for name, doc := range map[string]string{
		"no main module": "[modules.a]\nsource = \"a\"\n",
		"two main":       "[modules.a]\nsource = \"a\"\nmain = true\n[modules.b]\nsource = \"b\"\nmain = true\n",
		"main no source": "[modules.a]\nmain = true\n",
		"not toml":       "[[",
	} {
		if _, err := DevModuleSource([]byte(doc)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	target := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

func TestSnapshotAndRestoreReplaceTheProtectedPathsExactly(t *testing.T) {
	t.Parallel()
	approved := t.TempDir()
	write(t, approved, "dagger.toml", "approved toml\n")
	write(t, approved, ".dagger/eval/main.go", "approved eval\n")
	write(t, approved, ".dagger/eval/sub/helper.go", "approved helper\n")
	write(t, approved, "src/impl.go", "implementation\n")
	if err := os.Symlink("main.go", filepath.Join(approved, ".dagger/eval/link.go")); err != nil {
		t.Fatal(err)
	}
	copied, err := Snapshot(approved, []string{"dagger.toml", ".dagger/eval", "fixtures/missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(copied.Files) != 4 {
		t.Fatalf("%+v", copied.Files)
	}

	export := t.TempDir()
	write(t, export, "dagger.toml", "tampered\n")
	write(t, export, ".dagger/eval/main.go", "tampered eval\n")
	write(t, export, ".dagger/eval/cheat.go", "cheat\n")
	write(t, export, "fixtures/missing/new.txt", "added under a path that did not exist\n")
	write(t, export, "src/impl.go", "better implementation\n")
	if err := copied.Restore(t.Context(), export); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"dagger.toml": "approved toml\n", ".dagger/eval/main.go": "approved eval\n", ".dagger/eval/sub/helper.go": "approved helper\n", "src/impl.go": "better implementation\n"} {
		if got, _ := read(t, export, name); got != want {
			t.Errorf("%s is %q, want %q", name, got, want)
		}
	}
	for _, gone := range []string{".dagger/eval/cheat.go", "fixtures/missing/new.txt"} {
		if _, ok := read(t, export, gone); ok {
			t.Errorf("%s survived the restore", gone)
		}
	}
	if target, err := os.Readlink(filepath.Join(export, ".dagger/eval/link.go")); err != nil || target != "main.go" {
		t.Fatalf("symlink %q, %v", target, err)
	}
}

func TestRestoreNeverFollowsASymbolicLinkOutOfTheExport(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	write(t, outside, "main.go", "untouched\n")
	export := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(export, ".dagger")); err != nil {
		t.Fatal(err)
	}
	copied := ApprovedCopy{Paths: []string{".dagger/eval"}, Files: []ApprovedFile{{Path: ".dagger/eval/main.go", Mode: 0o644, Content: []byte("approved\n")}}}
	if err := copied.Restore(t.Context(), export); err == nil {
		t.Fatal("restore wrote through a symbolic link")
	}
	if got, _ := read(t, outside, "main.go"); got != "untouched\n" {
		t.Fatalf("outside file is %q", got)
	}
}

func TestRestoreRefusesFilesOutsideTheProtectedPaths(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]ApprovedCopy{
		"unlisted file": {Paths: []string{"a"}, Files: []ApprovedFile{{Path: "b/x", Content: []byte("x")}}},
		"escaping path": {Paths: []string{"../a"}},
		"escaping file": {Paths: []string{"a"}, Files: []ApprovedFile{{Path: "a/../../x"}}},
	} {
		if err := c.Restore(t.Context(), t.TempDir()); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
