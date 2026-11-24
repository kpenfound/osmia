package kb

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

var at = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
var owner = trace.Actor{Kind: "owner", ID: "local"}

func traceFixture(t *testing.T, seed []byte) (*trace.Repository, config.Root, config.Project) {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(base, "clone")
	if err := os.Mkdir(clone, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "--quiet", clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR="}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	p := config.Project{ID: "p_00000000000000000000000000000001", Clone: clone}
	var r *trace.Repository
	if seed == nil {
		r, err = trace.Create(context.Background(), root, p, at, owner)
	} else {
		r, err = trace.CreateSeeded(context.Background(), root, p, at, owner, seed)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, root, p
}

func entityRevisions(r *trace.Repository) ([]trace.Document, error) {
	documents, err := trace.Read[trace.Document](r, "")
	var out []trace.Document
	for _, d := range documents {
		if d.ID == trace.EntitiesDocument {
			out = append(out, d)
		}
	}
	return out, err
}

func TestStoreRecordsRevisions(t *testing.T) {
	r, _, _ := traceFixture(t, nil)
	m, err := Load(r)
	if err != nil || !reflect.DeepEqual(m, Map{Version: Version, Entities: []Entity{}}) {
		t.Fatalf("fresh trace: %#v, %v", m, err)
	}
	invalid := Map{Version: Version, Entities: []Entity{entity("a", []string{"../x"})}}
	if err := Store(context.Background(), r, invalid, at, owner, "test"); err == nil {
		t.Fatal("invalid map stored")
	}
	if docs, err := entityRevisions(r); err != nil || len(docs) != 0 {
		t.Fatalf("invalid map wrote documents: %v, %v", docs, err)
	}
	first := Map{Version: Version, Entities: []Entity{{ID: "a", Name: "A", Aliases: []string{}, Paths: []string{"a"}, Owners: []string{}, PartOf: []string{}}}}
	second := Map{Version: Version, Entities: append(append([]Entity{}, first.Entities...), Entity{ID: "b", Name: "B", Aliases: []string{}, Paths: []string{"b"}, Owners: []string{}, PartOf: []string{"a"}})}
	for _, m := range []Map{first, second} {
		if err := Store(context.Background(), r, m, at, owner, "test"); err != nil {
			t.Fatal(err)
		}
	}
	docs, err := entityRevisions(r)
	if err != nil || len(docs) != 2 {
		t.Fatalf("documents: %v, %v", docs, err)
	}
	firstData, _ := Encode(first)
	secondData, _ := Encode(second)
	for i, want := range []string{string(firstData), string(secondData)} {
		d := docs[i]
		if d.ID != trace.EntitiesDocument || d.Path != trace.EntitiesPath || d.Revision != i+1 || d.Content != want || d.Cause != "test" {
			t.Fatalf("revision %d: %#v", i+1, d)
		}
	}
	loaded, err := Load(r)
	if err != nil || !reflect.DeepEqual(loaded, second) {
		t.Fatalf("load: %#v, %v", loaded, err)
	}
	file := filepath.Join(t.TempDir(), "entities.json")
	if err := os.WriteFile(file, secondData, 0600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := LoadFile(file)
	if err != nil || !reflect.DeepEqual(fromFile, second) {
		t.Fatalf("load file: %#v, %v", fromFile, err)
	}
}

func TestCreateSeededIsFirstRevision(t *testing.T) {
	clone := t.TempDir()
	writeTree(t, clone, fixtureFiles)
	seed := seedBytes(t, clone)
	r, root, p := traceFixture(t, seed)
	docs, err := entityRevisions(r)
	if err != nil || len(docs) != 1 || docs[0].Revision != 1 || docs[0].Content != string(seed) || docs[0].Cause != "project-create" {
		t.Fatalf("documents: %#v, %v", docs, err)
	}
	m, err := Load(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := Store(context.Background(), r, m, at, owner, "regenerate"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := trace.Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	docs, err = entityRevisions(reopened)
	if err != nil || len(docs) != 2 || docs[1].Revision != 2 {
		t.Fatalf("after reopen: %#v, %v", docs, err)
	}
}
