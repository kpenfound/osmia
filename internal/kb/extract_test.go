package kb

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeOutput(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCheckSubsystem(t *testing.T) {
	for _, name := range []string{"trace", "a", "0", "entity-map-2", strings.Repeat("a", MaxSubsystemLength)} {
		if err := CheckSubsystem(name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}
	for _, name := range []string{"", "-trace", "Trace", "trace_repo", "trace.md", "a/b", "a b", "é", strings.Repeat("a", MaxSubsystemLength+1)} {
		if err := CheckSubsystem(name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
	if ProsePath("trace") != "kb/trace.md" || ProseDocument("trace") != "subsystem-trace" {
		t.Fatal(ProsePath("trace"), ProseDocument("trace"))
	}
	for path, want := range map[string]string{"kb/trace.md": "trace", "kb/entities.json": "", "kb/Trace.md": "", "kb/a/b.md": "", "trace.md": "", "kb/entities.md": "entities"} {
		if got, ok := Subsystem(path); got != want || ok != (want != "") {
			t.Errorf("Subsystem(%q) = %q, %t", path, got, ok)
		}
	}
}

func TestReadOutput(t *testing.T) {
	entities := "{\"version\":1,\"entities\":[{\"id\":\"internal.trace\",\"name\":\"internal/trace\",\"aliases\":[\"trace\"],\"paths\":[\"internal/trace\"],\"owners\":[],\"part_of\":[]}]}\n"
	dir := writeOutput(t, map[string]string{"kb/trace.md": "# trace\n", "kb/service.md": "# service\n", "kb/entities.json": entities})
	out, err := ReadOutput(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Subsystems(), []string{"service", "trace"}) || out.Prose["trace"] != "# trace\n" || len(out.Entities.Entities) != 1 || out.Entities.Entities[0].ID != "internal.trace" {
		t.Fatalf("%+v", out)
	}
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"missing directory", nil, []string{"no output was produced"}},
		{"missing kb", map[string]string{"README.md": "x"}, []string{"unexpected output entry \"README.md\"", "kb/ is missing"}},
		{"missing entities", map[string]string{"kb/trace.md": "# trace\n"}, []string{"kb/entities.json is missing"}},
		{"no prose", map[string]string{"kb/entities.json": entities}, []string{"no kb/<subsystem>.md prose was produced"}},
		{"bad name", map[string]string{"kb/Trace_Repo.md": "x\n", "kb/entities.json": entities}, []string{"kb/Trace_Repo.md", "^[a-z0-9][a-z0-9-]*$"}},
		{"not markdown", map[string]string{"kb/trace.txt": "x\n", "kb/entities.json": entities}, []string{"kb/trace.txt is not a subsystem file"}},
		{"nested", map[string]string{"kb/nested/trace.md": "x\n", "kb/entities.json": entities}, []string{"kb/nested is not a regular file"}},
		{"empty prose", map[string]string{"kb/trace.md": "\n\n", "kb/entities.json": entities}, []string{"kb/trace.md is empty"}},
		{"invalid entities", map[string]string{"kb/trace.md": "x\n", "kb/entities.json": "{\"version\":1,\"entities\":[{\"id\":\"Bad ID\",\"name\":\"\",\"aliases\":[],\"paths\":[\"/abs\"],\"owners\":[],\"part_of\":[\"missing\"]}]}"}, []string{"kb/entities.json", "id must be", "name is required", "absolute", "unknown entity"}},
		{"malformed entities", map[string]string{"kb/trace.md": "x\n", "kb/entities.json": "{\"version\":1,\"extra\":true}"}, []string{"kb/entities.json"}},
		{"several problems", map[string]string{"extra": "x", "kb/BAD.md": "x\n", "kb/trace.md": ""}, []string{"unexpected output entry \"extra\"", "kb/BAD.md", "kb/trace.md is empty", "kb/entities.json is missing"}},
	}
	for _, tc := range cases {
		dir := filepath.Join(t.TempDir(), "missing")
		if tc.files != nil {
			dir = writeOutput(t, tc.files)
		}
		_, err := ReadOutput(dir)
		if err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: %v lacks %q", tc.name, err, want)
			}
		}
	}
	// Symlinks are refused even when they point at valid content.
	dir = writeOutput(t, map[string]string{"kb/entities.json": entities, "real.md": "# real\n"})
	if err := os.Symlink(filepath.Join(dir, "real.md"), filepath.Join(dir, "kb", "trace.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOutput(dir); err == nil || !strings.Contains(err.Error(), "kb/trace.md is not a regular file") {
		t.Fatalf("symlink: %v", err)
	}
}
