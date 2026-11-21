package projectstore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

const (
	pidA = "p_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pidB = "p_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pidC = "p_cccccccccccccccccccccccccccccccc"
	pidD = "p_dddddddddddddddddddddddddddddddd"
)

// projectFile is a project file of one project: an owner's file with
// comments, some settings left to their defaults and a table.
func projectFile(n string) string {
	return "# Settings of project " + n + ".\nversion = 1\nupstream = \"upstream/" + n + "\"\nfork = \"owner/" + n + "\"\nclone = \"~/clone-" + n + "\"\nchecks_timeout = \"30m\"\n\n[capacity]\ngit_operations = 3\n"
}

// legacyRoot is an explicit root laid out as the version before the store
// left it: a top-level file listing projects and each project's file.
type legacyRoot struct {
	t    *testing.T
	home string
	dir  string
	opts config.Options
}

func newLegacyRoot(t *testing.T, list []string, files map[string]string) *legacyRoot {
	t.Helper()
	// A short path keeps the Unix socket the loader checks within its limit.
	base, err := os.MkdirTemp("", "ps-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	l := &legacyRoot{t: t, home: filepath.Join(base, "home"), dir: filepath.Join(base, "root")}
	l.opts = config.Options{Root: l.dir, Home: l.home}
	for _, d := range []string{l.home, l.dir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	quoted := make([]string, len(list))
	for i, id := range list {
		quoted[i] = `"` + id + `"`
	}
	top := "version = 1\nactive_projects = [" + strings.Join(quoted, ", ") + "]\n[profiles.default]\nagent = \"claude\"\nmodel = \"test-model\"\n"
	l.write(filepath.Join(l.dir, "config.toml"), top)
	for id, data := range files {
		l.write(filepath.Join(l.dir, "projects", id, "config.toml"), data)
	}
	return l
}

func (l *legacyRoot) write(path, data string) {
	l.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		l.t.Fatal(err)
	}
}

func (l *legacyRoot) topLevel() string { return filepath.Join(l.dir, "config.toml") }
func (l *legacyRoot) projectPath(id string) string {
	return filepath.Join(l.dir, "projects", id, "config.toml")
}

func (l *legacyRoot) config() *config.Config {
	l.t.Helper()
	cfg, err := config.LoadTopLevel(l.opts)
	if err != nil {
		l.t.Fatal(err)
	}
	return cfg
}

func (l *legacyRoot) store(options ...Option) *Store { return Open(l.config().Root, options...) }

// snapshot records every file and directory of the tree: kind, mode and bytes.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel] = "dir " + info.Mode().String()
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = info.Mode().String() + " " + info.ModTime().String() + "\n" + string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// outside drops the entries of snapshot under the store's directory.
func outside(snap map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range snap {
		if k != "project-store" && !strings.HasPrefix(k, "project-store"+string(filepath.Separator)) {
			out[k] = v
		}
	}
	return out
}

func ids(t *testing.T, s *Store) []string {
	t.Helper()
	list, err := s.Projects()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(list))
	for i, id := range list {
		out[i] = string(id)
	}
	return out
}

func imported(t *testing.T, s *Store) bool {
	t.Helper()
	ok, err := s.Imported()
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func mustImport(t *testing.T, l *legacyRoot) *Store {
	t.Helper()
	s := l.store()
	if err := s.Import(context.Background(), l.config(), l.home); err != nil {
		t.Fatal(err)
	}
	return s
}

func revisions(t *testing.T, s *Store, id string) []Revision {
	t.Helper()
	r, err := s.Revisions(config.ProjectID(id))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegistryKeepsInsertionOrderAcrossReopen(t *testing.T) {
	l := newLegacyRoot(t, []string{pidB, pidA}, map[string]string{pidA: projectFile("a"), pidB: projectFile("b")})
	mustImport(t, l)
	reopened := l.store()
	if got := ids(t, reopened); !slices.Equal(got, []string{pidB, pidA}) {
		t.Fatalf("imported order %v", got)
	}
	for _, id := range []string{pidD, pidC} {
		if err := reopened.Add(config.ProjectID(id)); err != nil {
			t.Fatal(err)
		}
	}
	if got := ids(t, l.store()); !slices.Equal(got, []string{pidB, pidA, pidD, pidC}) {
		t.Fatalf("order after adds %v", got)
	}
	if err := l.store().Add(pidB); err != nil {
		t.Fatal(err)
	}
	if err := l.store().Remove(pidA); err != nil {
		t.Fatal(err)
	}
	if err := l.store().Remove(pidA); err != nil {
		t.Fatal(err)
	}
	if got := ids(t, l.store()); !slices.Equal(got, []string{pidB, pidD, pidC}) {
		t.Fatalf("order after remove %v", got)
	}
}

func TestAddAndRemoveChangeOnlyTheRegistry(t *testing.T) {
	l := newLegacyRoot(t, []string{pidA}, map[string]string{pidA: projectFile("a")})
	s := mustImport(t, l)
	before := snapshot(t, l.dir)
	if err := s.Add(pidB); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(pidA); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, l.dir)
	if !reflect.DeepEqual(outside(before), outside(after)) {
		t.Fatalf("add and remove changed files outside the store")
	}
	for k, v := range before {
		if strings.HasPrefix(k, filepath.Join("project-store", "settings")) && after[k] != v {
			t.Fatalf("add and remove changed %s", k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok && k != filepath.Join("project-store") && !strings.HasPrefix(k, filepath.Join("project-store")) {
			t.Fatalf("new file %s", k)
		}
	}
	if len(revisions(t, s, pidA)) != 1 || len(revisions(t, s, pidB)) != 0 {
		t.Fatal("revisions changed")
	}
}

func TestRegistryNeedsTheImport(t *testing.T) {
	l := newLegacyRoot(t, nil, nil)
	s := l.store()
	if err := s.Add(pidA); !errors.Is(err, ErrNotImported) {
		t.Fatalf("add before import: %v", err)
	}
	if err := s.Remove(pidA); !errors.Is(err, ErrNotImported) {
		t.Fatalf("remove before import: %v", err)
	}
	if _, err := s.Apply(l.config(), l.home); !errors.Is(err, ErrNotImported) {
		t.Fatalf("apply before import: %v", err)
	}
	if imported(t, s) || len(ids(t, s)) != 0 {
		t.Fatal("a root with no registry is not imported")
	}
}

func TestEmptyRootImportsToAnEmptyRegistry(t *testing.T) {
	l := newLegacyRoot(t, nil, nil)
	s := mustImport(t, l)
	if !imported(t, s) || len(ids(t, s)) != 0 {
		t.Fatal("an empty root imports to an empty registry")
	}
	cfg, err := s.Apply(l.config(), l.home)
	if err != nil || len(cfg.Projects) != 0 || cfg.HasProject() {
		t.Fatalf("apply: %+v %v", cfg, err)
	}
}

func TestSaveRecordsARevisionOnlyForTheCurrentPin(t *testing.T) {
	l := newLegacyRoot(t, []string{pidA}, map[string]string{pidA: projectFile("a")})
	s := mustImport(t, l)
	cfg := l.config()
	first, err := s.Current(pidA)
	if err != nil || first.Number != 1 || first.Pin == "" {
		t.Fatalf("current %+v %v", first, err)
	}
	second, file, err := s.Save(cfg, l.home, pidA, first.Pin, config.ProjectEdit{Strings: map[string]string{"landing": "squash"}})
	if err != nil || second.Number != 2 || second.Pin == first.Pin || file.Project.Landing != "squash" {
		t.Fatalf("save: %+v %+v %v", second, file.Project, err)
	}
	if !strings.Contains(string(second.Data), "# Settings of project a.") || !strings.Contains(string(second.Data), `landing = "squash"`) {
		t.Fatalf("saved bytes are not a text edit:\n%s", second.Data)
	}
	if got, _ := s.Current(pidA); got.Pin != second.Pin || !bytes.Equal(got.Data, second.Data) {
		t.Fatalf("current after save %+v", got)
	}
	stale, other := snapshot(t, l.dir), config.ProjectEdit{Strings: map[string]string{"landing": "commit-per-unit"}}
	if _, _, err := s.Save(cfg, l.home, pidA, first.Pin, other); !errors.Is(err, ErrStale) {
		t.Fatalf("stale save: %v", err)
	}
	if _, err := s.Replace(pidA, first.Pin, []byte("version = 1\n")); !errors.Is(err, ErrStale) {
		t.Fatalf("stale replace: %v", err)
	}
	if _, _, err := s.Save(cfg, l.home, pidA, "", other); !errors.Is(err, ErrPinRequired) {
		t.Fatalf("save without a pin: %v", err)
	}
	if !reflect.DeepEqual(stale, snapshot(t, l.dir)) {
		t.Fatal("a refused save wrote something")
	}
	third, err := s.Replace(pidA, second.Pin, append(second.Data, "# note\n"...))
	if err != nil || third.Number != 3 {
		t.Fatalf("replace: %+v %v", third, err)
	}
	if got := revisions(t, s, pidA); len(got) != 3 || !bytes.Equal(got[0].Data, []byte(projectFile("a"))) {
		t.Fatalf("history %d revisions", len(got))
	}
	// An unchanged save is still a revision, and its pin differs.
	same, _, err := s.Save(cfg, l.home, pidA, third.Pin, config.ProjectEdit{Strings: map[string]string{"landing": "squash"}})
	if err != nil || same.Pin == third.Pin {
		t.Fatalf("unchanged save: %+v %v", same, err)
	}
}

func TestInvalidEditIsRefusedNamingTheField(t *testing.T) {
	l := newLegacyRoot(t, []string{pidA}, map[string]string{pidA: projectFile("a")})
	s := mustImport(t, l)
	current, _ := s.Current(pidA)
	before := snapshot(t, l.dir)
	for _, c := range []struct {
		edit  config.ProjectEdit
		field string
	}{
		{config.ProjectEdit{Strings: map[string]string{"base_branch": "bad..branch"}}, "base_branch"},
		{config.ProjectEdit{Strings: map[string]string{"upstream": "https://example.com/x.git"}}, "upstream"},
		{config.ProjectEdit{Strings: map[string]string{"checks_timeout": "1s"}}, "checks_timeout"},
		{config.ProjectEdit{Ints: map[string]int{"capacity.per_workstream": 0}}, "capacity.per_workstream"},
		{config.ProjectEdit{Clear: []string{"upstream"}}, "upstream"},
	} {
		_, _, err := s.Save(l.config(), l.home, pidA, current.Pin, c.edit)
		var field *config.FieldError
		if !errors.As(err, &field) || field.Field != c.field {
			t.Fatalf("edit of %s: %v", c.field, err)
		}
		// The same refusal today's decoding gives for the edited file.
		want := field.Error()
		if !strings.Contains(want, c.field) || strings.Contains(want, "example.com") {
			t.Fatalf("error text %q", want)
		}
	}
	if !reflect.DeepEqual(before, snapshot(t, l.dir)) {
		t.Fatal("a refused edit wrote something")
	}
}

func TestSaveOverARevisionThatDoesNotLoad(t *testing.T) {
	l := newLegacyRoot(t, []string{pidA}, map[string]string{pidA: projectFile("a")})
	s := mustImport(t, l)
	current, _ := s.Current(pidA)
	// The clone becomes a regular file after the settings were stored.
	clone := filepath.Join(l.home, "clone-a")
	if err := os.WriteFile(clone, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := l.config()
	if _, rev, err := s.Load(cfg, l.home, pidA); err == nil || rev.Pin != current.Pin {
		t.Fatalf("load of a revision that does not load: %+v %v", rev, err)
	}
	if _, err := s.Apply(cfg, l.home); err == nil {
		t.Fatal("apply loaded a project that does not load")
	}
	if _, _, err := s.Save(cfg, l.home, pidA, current.Pin, config.ProjectEdit{Strings: map[string]string{"landing": "squash"}}); err == nil {
		t.Fatal("saved a result that does not load")
	}
	fixed, file, err := s.Save(cfg, l.home, pidA, current.Pin, config.ProjectEdit{Strings: map[string]string{"clone": filepath.Join(l.home, "elsewhere")}})
	if err != nil || fixed.Number != 2 || file.Project.Clone != filepath.Join(l.home, "elsewhere") {
		t.Fatalf("repair: %+v %v", fixed, err)
	}
	if _, _, err := s.Load(cfg, l.home, pidA); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFirstIsRevisionOneAndRepeatable(t *testing.T) {
	l := newLegacyRoot(t, nil, nil)
	s := mustImport(t, l)
	data, err := config.ProjectConfigBytes(config.Project{Name: "Shop", Upstream: "owner/shop", Clone: filepath.Join(l.home, "shop"), BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.WriteFirst(pidA, data)
	if err != nil || first.Number != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	again, err := s.WriteFirst(pidA, []byte("version = 1\n"))
	if err != nil || again.Pin != first.Pin || len(revisions(t, s, pidA)) != 1 {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	file, _, err := s.Load(l.config(), l.home, pidA)
	if err != nil || file.Project.Name != "Shop" || !file.Set("landing") || file.Set("checks_timeout") {
		t.Fatalf("load: %+v %v", file.Project, err)
	}
	if _, err := s.Current(pidB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("project without settings: %v", err)
	}
}

func TestLoadFromTheStoreMatchesLoadingTheFiles(t *testing.T) {
	files := map[string]string{
		pidA: projectFile("a"),
		pidB: "version = 1\nname = \"B\"\nupstream = \"upstream/b\"\nclone = \"/tmp/never-here-b\"\nlanding = \"squash\"\ndelivery = \"jev\"\nflake_fixes = true\nhearsay_scope = \"work:osmia:p_bbbb\"\n\n[hearsay_entities]\nshop = \"code:owner/b:shop\"\n",
		pidC: "version = 1\nupstream = \"upstream/c\"\nclone = \"/tmp/never-here-c\"\nupstream_rebase = \"0\"\n",
	}
	l := newLegacyRoot(t, []string{pidA, pidB, pidC}, files)
	want, err := config.Load(l.opts)
	if err != nil {
		t.Fatal(err)
	}
	s := mustImport(t, l)
	got, err := s.Apply(l.config(), l.home)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want.Projects, got.Projects) || !reflect.DeepEqual(want.ActiveProjects, got.ActiveProjects) {
		t.Fatalf("projects differ:\nfile  %+v\nstore %+v", want.Projects, got.Projects)
	}
	if got.HasProject() {
		t.Fatal("several projects have no single project")
	}
	for id, data := range files {
		fromFile, err := l.config().DecodeProject(config.ProjectID(id), l.home, []byte(data))
		if err != nil {
			t.Fatal(err)
		}
		fromStore, _, err := s.Load(l.config(), l.home, config.ProjectID(id))
		if err != nil {
			t.Fatal(err)
		}
		for _, setting := range config.ProjectSettings {
			if fromFile.Set(setting) != fromStore.Set(setting) {
				t.Fatalf("%s: marker of %s differs", id, setting)
			}
		}
	}
	// One project is the Project of the loaded configuration, as it is from files.
	solo := newLegacyRoot(t, []string{pidA}, map[string]string{pidA: projectFile("a")})
	soloWant, err := config.Load(solo.opts)
	if err != nil {
		t.Fatal(err)
	}
	soloGot, err := mustImport(t, solo).Apply(solo.config(), solo.home)
	if err != nil || !reflect.DeepEqual(soloWant.Project, soloGot.Project) || !soloGot.HasProject() {
		t.Fatalf("single project: %+v %v", soloGot.Project, err)
	}
}

func TestImportBringsInTheListAndEveryListedFile(t *testing.T) {
	files := map[string]string{pidA: projectFile("a"), pidB: projectFile("b"), pidC: projectFile("c"), pidD: "this is not toml ["}
	l := newLegacyRoot(t, []string{pidC, pidA, pidB}, files)
	// pidD is a project directory the list does not name: not read, not changed.
	l.write(filepath.Join(l.dir, "projects", pidD, "trace.db"), "database")
	cfg := l.config()
	before := snapshot(t, l.dir)
	s := mustImport(t, l)
	if got := ids(t, s); !slices.Equal(got, []string{pidC, pidA, pidB}) || !imported(t, s) {
		t.Fatalf("registry %v", got)
	}
	retired := 0
	for _, id := range []string{pidA, pidB, pidC} {
		revs := revisions(t, s, id)
		if len(revs) != 1 || string(revs[0].Data) != files[id] {
			t.Fatalf("%s: revisions %+v", id, revs)
		}
		data, err := os.ReadFile(l.projectPath(id))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(data), RetiredMark) || strings.Count(string(data), "Retired:") != 1 {
			t.Fatalf("%s: no single retired mark:\n%s", id, data)
		}
		if strings.TrimPrefix(string(data), RetiredMark) != files[id] {
			t.Fatalf("%s: the file lost bytes", id)
		}
		// The file still loads under the project-file rules, to the same settings.
		marked, err := cfg.DecodeProject(config.ProjectID(id), l.home, data)
		original, _ := cfg.DecodeProject(config.ProjectID(id), l.home, []byte(files[id]))
		if err != nil || !reflect.DeepEqual(marked.Project, original.Project) {
			t.Fatalf("%s: retired file decodes to %+v: %v", id, marked.Project, err)
		}
		for _, setting := range config.ProjectSettings {
			if marked.Set(setting) != original.Set(setting) {
				t.Fatalf("%s: retired file changes the marker of %s", id, setting)
			}
		}
		retired++
	}
	after := snapshot(t, l.dir)
	for _, id := range []string{pidD} {
		for k, v := range before {
			if strings.Contains(k, id) && after[k] != v {
				t.Fatalf("unlisted project changed: %s", k)
			}
		}
	}
	if before["config.toml"] != after["config.toml"] {
		t.Fatal("the top-level file changed")
	}
	if retired != 3 {
		t.Fatal(retired)
	}
}

// failureRoot lists three projects, the second of which does not load.
func failureRoot(t *testing.T) *legacyRoot {
	bad := strings.Replace(projectFile("b"), `fork = "owner/b"`, `fork = "not a repository"`, 1)
	l := newLegacyRoot(t, []string{pidA, pidB, pidC}, map[string]string{pidA: projectFile("a"), pidB: bad, pidC: projectFile("c")})
	l.write(filepath.Join(l.dir, "projects", pidD, "config.toml"), "version = 1\ngroup_id = \""+pidA+"\"\nupstream = \"upstream/d\"\nclone = \"~/d\"\n")
	return l
}

func requireFieldError(t *testing.T, err error, id, field string) {
	t.Helper()
	var fe *config.FieldError
	if !errors.As(err, &fe) || fe.Field != field || !strings.Contains(fe.Path, id) || !strings.HasSuffix(fe.Path, "config.toml") {
		t.Fatalf("error %v, want a field error for %s of %s", err, field, id)
	}
	if strings.Contains(err.Error(), "not a repository") {
		t.Fatalf("error quotes a value: %v", err)
	}
}

func TestPreflightNamesTheFileAndFieldAndChangesNothing(t *testing.T) {
	l := failureRoot(t)
	before := snapshot(t, l.dir)
	s := l.store()
	requireFieldError(t, s.Preflight(l.config(), l.home), pidB, "fork")
	if !reflect.DeepEqual(before, snapshot(t, l.dir)) {
		t.Fatal("the preflight changed the root")
	}
	if _, err := os.Stat(filepath.Join(l.dir, "project-store")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the preflight created the store")
	}
	// A listed file that is missing or unreadable is named too.
	l = newLegacyRoot(t, []string{pidA}, nil)
	err := l.store().Preflight(l.config(), l.home)
	var fe *config.FieldError
	if !errors.As(err, &fe) || !strings.Contains(fe.Path, pidA) || fe.Reason != "cannot be read" {
		t.Fatalf("missing file: %v", err)
	}
}

func TestPreflightPassesWhenEveryListedFileLoads(t *testing.T) {
	l := newLegacyRoot(t, []string{pidA, pidB}, map[string]string{pidA: projectFile("a"), pidB: projectFile("b")})
	before := snapshot(t, l.dir)
	if err := l.store().Preflight(l.config(), l.home); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshot(t, l.dir)) {
		t.Fatal("the preflight changed the root")
	}
}

func TestImportRefusesAFileThatDoesNotLoadAndRecordsNothing(t *testing.T) {
	l := failureRoot(t)
	before := snapshot(t, l.dir)
	s := l.store()
	requireFieldError(t, s.Import(context.Background(), l.config(), l.home), pidB, "fork")
	if imported(t, s) || len(ids(t, s)) != 0 {
		t.Fatal("a refused import recorded the registry")
	}
	for _, id := range []string{pidA, pidB, pidC} {
		if len(revisions(t, s, id)) != 0 {
			t.Fatalf("%s: a refused import recorded settings", id)
		}
	}
	if !reflect.DeepEqual(outside(before), outside(snapshot(t, l.dir))) {
		t.Fatal("a refused import changed a source file")
	}
}

func TestInterruptedImportResumesWithoutDuplicates(t *testing.T) {
	files := map[string]string{pidA: projectFile("a"), pidB: projectFile("b"), pidC: projectFile("c")}
	list := []string{pidB, pidC, pidA}
	var steps []string
	record := newLegacyRoot(t, list, files)
	if err := record.store(WithStep(func(name string) error { steps = append(steps, name); return nil })).Import(context.Background(), record.config(), record.home); err != nil {
		t.Fatal(err)
	}
	want := []string{"import-checked", "registry-staged", "import-recorded"}
	for _, w := range want {
		if !slices.Contains(steps, w) {
			t.Fatalf("steps %v lack %s", steps, w)
		}
	}
	for _, id := range list {
		for _, w := range []string{"revision-staged:" + id + ":1", "revision-recorded:" + id + ":1", "settings-imported:" + id, "file-retired:" + id} {
			if !slices.Contains(steps, w) {
				t.Fatalf("steps %v lack %s", steps, w)
			}
		}
	}
	crash := errors.New("crash")
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			l := newLegacyRoot(t, list, files)
			err := l.store(WithStep(func(name string) error {
				if name == step {
					return crash
				}
				return nil
			})).Import(context.Background(), l.config(), l.home)
			if !errors.Is(err, crash) {
				t.Fatalf("interrupted import: %v", err)
			}
			if step != "import-recorded" && imported(t, l.store()) {
				t.Fatal("an interrupted import is recorded as complete")
			}
			var ran []string
			s := l.store(WithStep(func(name string) error { ran = append(ran, name); return nil }))
			if err := s.Import(context.Background(), l.config(), l.home); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if !imported(t, s) || !slices.Equal(ids(t, s), list) {
				t.Fatalf("resumed registry %v", ids(t, s))
			}
			for id, data := range files {
				revs := revisions(t, s, id)
				if len(revs) != 1 || string(revs[0].Data) != data {
					t.Fatalf("%s: %d revisions after resume", id, len(revs))
				}
				got, _ := os.ReadFile(l.projectPath(id))
				if string(got) != RetiredMark+data {
					t.Fatalf("%s: retired file:\n%s", id, got)
				}
			}
			filepath.WalkDir(l.dir, func(path string, d fs.DirEntry, err error) error {
				if err == nil && strings.HasPrefix(d.Name(), tempPrefix) {
					t.Errorf("temporary file %s left behind", path)
				}
				return nil
			})
			// A completed import is a no-op.
			done := snapshot(t, l.dir)
			ran = nil
			if err := s.Import(context.Background(), l.config(), l.home); err != nil || len(ran) != 0 {
				t.Fatalf("second import: %v, steps %v", err, ran)
			}
			if !reflect.DeepEqual(done, snapshot(t, l.dir)) {
				t.Fatal("a completed import changed the root")
			}
		})
	}
}

func TestCompletedImportReadsNoProjectFile(t *testing.T) {
	files := map[string]string{pidA: projectFile("a")}
	l := newLegacyRoot(t, []string{pidA}, files)
	s := mustImport(t, l)
	cfg := l.config()
	// The retired file is unreadable, then invalid; neither matters.
	for _, content := range []string{"", "not = [toml"} {
		l.write(l.projectPath(pidA), content)
		if err := os.Chmod(l.projectPath(pidA), 0); err != nil {
			t.Fatal(err)
		}
		if err := s.Preflight(cfg, l.home); err != nil {
			t.Fatal(err)
		}
		if err := s.Import(context.Background(), cfg, l.home); err != nil {
			t.Fatal(err)
		}
		applied, err := s.Apply(cfg, l.home)
		if err != nil || len(applied.Projects) != 1 || applied.Project.Upstream != "upstream/a" {
			t.Fatalf("apply: %+v %v", applied, err)
		}
		os.Chmod(l.projectPath(pidA), 0600)
	}
}

func TestConcurrentImportsRecordOneRevisionPerProject(t *testing.T) {
	files := map[string]string{pidA: projectFile("a"), pidB: projectFile("b")}
	l := newLegacyRoot(t, []string{pidA, pidB}, files)
	cfg := l.config()
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Open(cfg.Root).Import(context.Background(), cfg, l.home)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	s := l.store()
	for id, data := range files {
		if revs := revisions(t, s, id); len(revs) != 1 || string(revs[0].Data) != data {
			t.Fatalf("%s: %d revisions", id, len(revs))
		}
	}
	if !slices.Equal(ids(t, s), []string{pidA, pidB}) {
		t.Fatal(ids(t, s))
	}
}

func TestNoOperationWritesTheTopLevelFile(t *testing.T) {
	files := map[string]string{pidA: projectFile("a"), pidB: projectFile("b")}
	l := newLegacyRoot(t, []string{pidA, pidB}, files)
	top := l.topLevel()
	if err := os.Chmod(top, 0400); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, l.dir)
	cfg := l.config()
	exercise := func() {
		s := Open(cfg.Root)
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		must(s.Preflight(cfg, l.home))
		must(s.Import(context.Background(), cfg, l.home))
		must(s.Add(pidC))
		must(s.Remove(pidA))
		current, err := s.Current(pidB)
		must(err)
		_, _, err = s.Save(cfg, l.home, pidB, current.Pin, config.ProjectEdit{Strings: map[string]string{"landing": "squash"}})
		must(err)
		_, err = s.WriteFirst(pidC, []byte(files[pidA]))
		must(err)
		_, err = s.Apply(cfg, l.home)
		must(err)
	}
	exercise()
	if got, want := snapshot(t, l.dir)["config.toml"], before["config.toml"]; got != want {
		t.Fatal("the top-level file changed")
	}
	// Only the store and the imported project files are written.
	for k, v := range outside(snapshot(t, l.dir)) {
		if before[k] != v && k != filepath.Join("projects", pidA, "config.toml") && k != filepath.Join("projects", pidB, "config.toml") {
			t.Fatalf("%s changed", k)
		}
	}
	for k := range outside(snapshot(t, l.dir)) {
		if _, ok := before[k]; !ok {
			t.Fatalf("%s is new outside the store", k)
		}
	}
	// With the top-level file gone, nothing recreates it.
	if err := os.Remove(top); err != nil {
		t.Fatal(err)
	}
	s := Open(cfg.Root)
	if err := s.Add(pidD); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Current(pidB)
	if _, err := s.Replace(pidB, current.Pin, []byte(files[pidB])); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(top); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the top-level file was created")
	}
}

func TestStoreAddsNothingToProjectDirectories(t *testing.T) {
	l := newLegacyRoot(t, []string{pidA}, map[string]string{pidA: projectFile("a")})
	l.write(filepath.Join(l.dir, "projects", pidA, "trace.db"), "database")
	l.write(filepath.Join(l.dir, "projects", pidA, "osmia.lock"), "")
	s := mustImport(t, l)
	current, _ := s.Current(pidA)
	if _, err := s.Replace(pidA, current.Pin, []byte(projectFile("a"))); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(l.dir, "projects", pidA))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"config.toml", "osmia.lock", "trace.db"}) {
		t.Fatalf("project directory holds %v", names)
	}
	top, err := os.ReadDir(l.dir)
	if err != nil {
		t.Fatal(err)
	}
	names = nil
	for _, e := range top {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"config.toml", "project-store", "projects"}) {
		t.Fatalf("root holds %v", names)
	}
}
