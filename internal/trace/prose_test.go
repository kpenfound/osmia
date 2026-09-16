package trace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProseValidatesSubsystemNames(t *testing.T) {
	r, root, p := create(t)
	kb := filepath.Join(root.String(), "projects", string(p.ID), "kb")
	if err := os.WriteFile(filepath.Join(kb, "api.md"), []byte("API\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Prose("api"); err != nil || got != "API\n" {
		t.Fatalf("prose %q %v", got, err)
	}
	if _, err := r.Prose("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing prose: %v", err)
	}
	for _, name := range []string{"", "../charter", "a/b", ".hidden", "..", " api", "a:b", "a\\b"} {
		if _, err := r.Prose(name); err == nil || !strings.Contains(err.Error(), "invalid subsystem") {
			t.Errorf("Prose(%q): %v", name, err)
		}
	}
}

func TestSubsystemsListsProseNames(t *testing.T) {
	r, root, p := create(t)
	kb := filepath.Join(root.String(), "projects", string(p.ID), "kb")
	if got, err := r.Subsystems(); err != nil || !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("empty kb: %v %v", got, err)
	}
	for _, name := range []string{"zeta.md", "api.md", ".hidden.md", "notes.txt", ".md", "a:b.md"} {
		if err := os.WriteFile(filepath.Join(kb, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(kb, "dir.md"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("api.md", filepath.Join(kb, "link.md")); err != nil {
		t.Fatal(err)
	}
	got, err := r.Subsystems()
	if want := []string{"api", "dir", "link", "zeta"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("subsystems %v %v", got, err)
	}
	for _, name := range []string{"dir", "link"} {
		if _, err := r.Prose(name); err == nil {
			t.Errorf("irregular prose %s was read", name)
		}
	}
	if err := os.RemoveAll(kb); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Subsystems(); err != nil || !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("missing kb: %v %v", got, err)
	}
}
