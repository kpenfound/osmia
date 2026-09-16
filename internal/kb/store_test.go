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

func entitiesFile(t *testing.T, root config.Root, p config.Project) string {
	t.Helper()
	dir, err := root.ProjectTrace(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, trace.EntitiesPath))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// entityRevisions returns the entity map's revisions, leaving out other
// project documents such as the charter.
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
	r, root, p := traceFixture(t, nil)
	m, err := Load(r)
	if err != nil || !reflect.DeepEqual(m, Map{Version: Version, Entities: []Entity{}}) {
		t.Fatalf("fresh trace: %#v, %v", m, err)
	}
	if got := entitiesFile(t, root, p); got != "{}\n" {
		t.Fatalf("fresh file: %q", got)
	}
	invalid := Map{Version: Version, Entities: []Entity{entity("a", []string{"../x"})}}
	if err := Store(context.Background(), r, invalid, at, owner, "test"); err == nil {
		t.Fatal("invalid map stored")
	}
	if docs, err := entityRevisions(r); err != nil || len(docs) != 0 {
		t.Fatalf("invalid map wrote documents: %v, %v", docs, err)
	}
	if got := entitiesFile(t, root, p); got != "{}\n" {
		t.Fatalf("invalid map wrote the file: %q", got)
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
	if got := entitiesFile(t, root, p); got != string(secondData) {
		t.Fatalf("file: %q", got)
	}
	loaded, err := Load(r)
	if err != nil || !reflect.DeepEqual(loaded, second) {
		t.Fatalf("load: %#v, %v", loaded, err)
	}
	fromFile, err := LoadFile(filepath.Join(root.String(), "projects", string(p.ID), trace.EntitiesPath))
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
	if got := entitiesFile(t, root, p); got != string(seed) {
		t.Fatalf("file: %q", got)
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

func TestMergeKeepsIDs(t *testing.T) {
	clone := t.TempDir()
	writeTree(t, clone, fixtureFiles)
	seed, err := Seed(clone)
	if err != nil {
		t.Fatal(err)
	}
	// Regenerating an unchanged map is a no-op.
	merged := Merge(seed, seed)
	if a, b := mustEncode(t, merged), mustEncode(t, seed); a != b {
		t.Fatalf("merge of identical maps changed it:\n%s", a)
	}
	// The owner renames an entity, adds one by hand and the clone gains and
	// loses directories.
	edited := seed
	edited.Entities = nil
	for _, e := range seed.Entities {
		switch e.ID {
		case "internal.trace":
			e.ID, e.Name, e.Aliases = "trace-store", "Trace store", []string{"ledger"}
		case "internal.kb":
			e.PartOf = []string{}
		}
		edited.Entities = append(edited.Entities, e)
	}
	edited.Entities = append(edited.Entities, Entity{ID: "hand", Name: "Hand-made", Aliases: []string{"lib"}, Paths: []string{"**/*.proto"}})
	if err := Validate(edited); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(clone, "web")); err != nil {
		t.Fatal(err)
	}
	codeowners := fixtureFiles[".github/CODEOWNERS"] + "/internal/trace/store/ @org/store\n"
	writeTree(t, clone, map[string]string{".github/CODEOWNERS": codeowners, "internal/trace/store/": "", "internal/lib/": "", "internal/ledger/": "", "internal/Web-UI/": ""})
	fresh, err := Seed(clone)
	if err != nil {
		t.Fatal(err)
	}
	merged = Merge(edited, fresh)
	if err := Validate(merged); err != nil {
		t.Fatal(err)
	}
	got := map[string]Entity{}
	for _, e := range merged.Entities {
		got[e.Primary()] = e
	}
	for primary, want := range map[string]Entity{
		"internal/trace":       {ID: "trace-store", Name: "Trace store", Aliases: []string{"ledger"}},
		"internal/kb":          {ID: "internal.kb", Name: "internal/kb", Aliases: []string{"kb"}},
		"web":                  {ID: "web", Name: "web", Aliases: []string{}},
		"**/*.proto":           {ID: "hand", Name: "Hand-made", Aliases: []string{"lib"}},
		"internal/lib":         {ID: "internal.lib", Name: "internal/lib", Aliases: []string{}},
		"internal/ledger":      {ID: "internal.ledger", Name: "internal/ledger", Aliases: []string{}},
		"internal/Web-UI":      {ID: "internal.web-ui-2", Name: "internal/Web-UI", Aliases: []string{"Web-UI"}},
		"internal/Web_UI":      {ID: "internal.web-ui", Name: "internal/Web_UI", Aliases: []string{"Web_UI"}},
		"internal/trace/store": {ID: "internal.trace.store", Name: "internal/trace/store", Aliases: []string{"store"}},
	} {
		e, ok := got[primary]
		if !ok || e.ID != want.ID || e.Name != want.Name || !reflect.DeepEqual(e.Aliases, want.Aliases) {
			t.Errorf("%s: got %#v, want %#v", primary, e, want)
		}
	}
	if e := got["internal/kb"]; len(e.PartOf) != 0 {
		t.Errorf("hand-edited part_of replaced: %v", e.PartOf)
	}
	if len(merged.Entities) != len(edited.Entities)+4 {
		t.Errorf("merged %d entities, want %d", len(merged.Entities), len(edited.Entities)+4)
	}
	// Seeded entities below a renamed one point at its kept ID.
	fp := merged.ResolveEntities([]string{"trace-store"})
	if !reflect.DeepEqual(fp.Paths, []string{"internal/trace", "internal/trace/store"}) || !reflect.DeepEqual(got["internal/trace/store"].PartOf, []string{"trace-store"}) {
		t.Fatalf("renamed entity footprint: %#v", fp)
	}
	// Regenerating the regenerated map changes nothing.
	if a, b := mustEncode(t, Merge(merged, fresh)), mustEncode(t, merged); a != b {
		t.Fatal("second regeneration changed the map")
	}
}

func mustEncode(t *testing.T, m Map) string {
	t.Helper()
	data, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
