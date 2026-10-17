package trace

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
)

func TestProseValidatesSubsystemNames(t *testing.T) {
	r, _, _ := create(t)
	if err := r.RecordDocuments(context.Background(), []Document{projectDocument("subsystem-api", "kb/api.md", "API\n", 1)}); err != nil {
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
	r, _, _ := create(t)
	ctx := context.Background()
	if got, err := r.Subsystems(); err != nil || !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("empty kb: %v %v", got, err)
	}
	var docs []Document
	for _, name := range []string{"zeta", "api", "gone"} {
		docs = append(docs, projectDocument("subsystem-"+name, "kb/"+name+".md", "# "+name+"\n", 1))
	}
	if err := r.RecordDocuments(ctx, docs); err != nil {
		t.Fatal(err)
	}
	// An empty revision removes a subsystem.
	if err := r.RecordDocuments(ctx, []Document{projectDocument("subsystem-gone", "kb/gone.md", "", 2)}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Subsystems()
	if want := []string{"api", "zeta"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("subsystems %v %v", got, err)
	}
	if _, err := r.Prose("gone"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed prose: %v", err)
	}
}
