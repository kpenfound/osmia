package projectstore

import (
	"bytes"
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

func settingsOf(n string) string {
	return "# Settings of project " + n + ".\nversion = 1\nupstream = \"upstream/" + n + "\"\nfork = \"owner/" + n + "\"\nclone = \"~/clone-" + n + "\"\nchecks_timeout = \"30m\"\n\n[capacity]\ngit_operations = 3\n"
}

type testRoot struct {
	t    *testing.T
	home string
	dir  string
	opts config.Options
}

func newRoot(t *testing.T) *testRoot {
	t.Helper()
	base, err := os.MkdirTemp("", "ps-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	r := &testRoot{t: t, home: filepath.Join(base, "home"), dir: filepath.Join(base, "root")}
	r.opts = config.Options{Root: r.dir, Home: r.home}
	for _, d := range []string{r.home, r.dir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	top := "version = 1\n[profiles.default]\nagent = \"claude\"\nmodel = \"test-model\"\n"
	if err := os.WriteFile(filepath.Join(r.dir, "config.toml"), []byte(top), 0600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *testRoot) config() *config.Config {
	r.t.Helper()
	cfg, err := config.LoadTopLevel(r.opts)
	if err != nil {
		r.t.Fatal(err)
	}
	return cfg
}

func (r *testRoot) store(options ...Option) *Store { return Open(r.config().Root, options...) }

func (r *testRoot) register(id, settings string) {
	r.t.Helper()
	s := r.store()
	if _, err := s.WriteFirst(config.ProjectID(id), []byte(settings)); err != nil {
		r.t.Fatal(err)
	}
	if err := s.Add(config.ProjectID(id)); err != nil {
		r.t.Fatal(err)
	}
}

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

func revisions(t *testing.T, s *Store, id string) []Revision {
	t.Helper()
	r, err := s.Revisions(config.ProjectID(id))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegistryKeepsInsertionOrderAcrossReopen(t *testing.T) {
	r := newRoot(t)
	r.register(pidB, settingsOf("b"))
	r.register(pidA, settingsOf("a"))
	if got := ids(t, r.store()); !slices.Equal(got, []string{pidB, pidA}) {
		t.Fatalf("order %v", got)
	}
	for _, id := range []string{pidD, pidC} {
		if err := r.store().Add(config.ProjectID(id)); err != nil {
			t.Fatal(err)
		}
	}
	if got := ids(t, r.store()); !slices.Equal(got, []string{pidB, pidA, pidD, pidC}) {
		t.Fatalf("order after adds %v", got)
	}
}

func TestAddOfARegisteredProjectKeepsItsPlace(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	r.register(pidB, settingsOf("b"))
	if err := r.store().Add(pidA); err != nil {
		t.Fatal(err)
	}
	if got := ids(t, r.store()); !slices.Equal(got, []string{pidA, pidB}) {
		t.Fatalf("order %v", got)
	}
}

func TestRemoveTakesAProjectOffTheRegistryAndKeepsItsRevisions(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	r.register(pidB, settingsOf("b"))
	r.register(pidC, settingsOf("c"))
	for range 2 {
		if err := r.store().Remove(pidB); err != nil {
			t.Fatal(err)
		}
	}
	s := r.store()
	if got := ids(t, s); !slices.Equal(got, []string{pidA, pidC}) {
		t.Fatalf("order after remove %v", got)
	}
	if len(revisions(t, s, pidB)) != 1 {
		t.Fatal("removing a project dropped its revisions")
	}
}

func TestAddAndRemoveChangeOnlyTheRegistry(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	s := r.store()
	before := snapshot(t, r.dir)
	if err := s.Add(pidB); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(pidA); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, r.dir)
	for k, v := range after {
		if k == "project-store" || k == filepath.Join("project-store", "registry.json") || k == filepath.Join("project-store", "lock") {
			continue
		}
		if before[k] != v {
			t.Fatalf("%s changed", k)
		}
	}
	if len(revisions(t, s, pidA)) != 1 || len(revisions(t, s, pidB)) != 0 {
		t.Fatal("revisions changed")
	}
}

func TestSaveRecordsARevisionOnlyForTheCurrentPin(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	s, cfg := r.store(), r.config()
	first, err := s.Current(pidA)
	if err != nil || first.Number != 1 || first.Pin == "" {
		t.Fatalf("current %+v %v", first, err)
	}
	second, file, err := s.Save(cfg, r.home, pidA, first.Pin, config.ProjectEdit{Strings: map[string]string{"landing": "squash"}})
	if err != nil || second.Number != 2 || second.Pin == first.Pin || file.Project.Landing != "squash" {
		t.Fatalf("save: %+v %+v %v", second, file.Project, err)
	}
	if !strings.Contains(string(second.Data), "# Settings of project a.") || !strings.Contains(string(second.Data), `landing = "squash"`) {
		t.Fatalf("saved bytes are not a text edit:\n%s", second.Data)
	}
	if got, _ := s.Current(pidA); got.Pin != second.Pin || !bytes.Equal(got.Data, second.Data) {
		t.Fatalf("current after save %+v", got)
	}
	third, err := s.Replace(pidA, second.Pin, append(second.Data, "# note\n"...))
	if err != nil || third.Number != 3 {
		t.Fatalf("replace: %+v %v", third, err)
	}
	if got := revisions(t, s, pidA); len(got) != 3 || !bytes.Equal(got[0].Data, []byte(settingsOf("a"))) {
		t.Fatalf("history %d revisions", len(got))
	}
}

func TestSaveWithAStaleOrMissingPinWritesNothing(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	s, cfg := r.store(), r.config()
	first, _ := s.Current(pidA)
	if _, _, err := s.Save(cfg, r.home, pidA, first.Pin, config.ProjectEdit{Strings: map[string]string{"landing": "squash"}}); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, r.dir)
	other := config.ProjectEdit{Strings: map[string]string{"landing": "commit-per-unit"}}
	if _, _, err := s.Save(cfg, r.home, pidA, first.Pin, other); !errors.Is(err, ErrStale) {
		t.Fatalf("stale save: %v", err)
	}
	if _, err := s.Replace(pidA, first.Pin, []byte("version = 1\n")); !errors.Is(err, ErrStale) {
		t.Fatalf("stale replace: %v", err)
	}
	if _, _, err := s.Save(cfg, r.home, pidA, "", other); !errors.Is(err, ErrPinRequired) {
		t.Fatalf("save without a pin: %v", err)
	}
	if !reflect.DeepEqual(before, snapshot(t, r.dir)) {
		t.Fatal("a refused save wrote something")
	}
}

func TestUnchangedSaveIsStillARevisionWithANewPin(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	s, cfg := r.store(), r.config()
	first, _ := s.Current(pidA)
	edit := config.ProjectEdit{Strings: map[string]string{"landing": "squash"}}
	second, _, err := s.Save(cfg, r.home, pidA, first.Pin, edit)
	if err != nil {
		t.Fatal(err)
	}
	same, _, err := s.Save(cfg, r.home, pidA, second.Pin, edit)
	if err != nil || same.Pin == second.Pin || same.Number != 3 {
		t.Fatalf("unchanged save: %+v %v", same, err)
	}
}

func TestInvalidEditIsRefusedNamingTheField(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	s := r.store()
	current, _ := s.Current(pidA)
	before := snapshot(t, r.dir)
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
		_, _, err := s.Save(r.config(), r.home, pidA, current.Pin, c.edit)
		var field *config.FieldError
		if !errors.As(err, &field) || field.Field != c.field {
			t.Fatalf("edit of %s: %v", c.field, err)
		}
		if text := field.Error(); !strings.Contains(text, c.field) || strings.Contains(text, "example.com") {
			t.Fatalf("error text %q", text)
		}
	}
	if !reflect.DeepEqual(before, snapshot(t, r.dir)) {
		t.Fatal("a refused edit wrote something")
	}
}

func TestSaveOverARevisionThatDoesNotLoad(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	s := r.store()
	current, _ := s.Current(pidA)
	clone := filepath.Join(r.home, "clone-a")
	if err := os.WriteFile(clone, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := r.config()
	if _, rev, err := s.Load(cfg, r.home, pidA); err == nil || rev.Pin != current.Pin {
		t.Fatalf("load of a revision that does not load: %+v %v", rev, err)
	}
	if _, _, err := s.Save(cfg, r.home, pidA, current.Pin, config.ProjectEdit{Strings: map[string]string{"landing": "squash"}}); err == nil {
		t.Fatal("saved a result that does not load")
	}
	elsewhere := filepath.Join(r.home, "elsewhere")
	fixed, file, err := s.Save(cfg, r.home, pidA, current.Pin, config.ProjectEdit{Strings: map[string]string{"clone": elsewhere}})
	if err != nil || fixed.Number != 2 || file.Project.Clone != elsewhere {
		t.Fatalf("repair: %+v %v", fixed, err)
	}
	if _, _, err := s.Load(cfg, r.home, pidA); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFirstIsRevisionOneAndRepeatable(t *testing.T) {
	r := newRoot(t)
	s := r.store()
	data, err := config.ProjectConfigBytes(config.Project{Name: "Shop", Upstream: "owner/shop", Clone: filepath.Join(r.home, "shop"), BaseBranch: "main"})
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
	file, _, err := s.Load(r.config(), r.home, pidA)
	if err != nil || file.Project.Name != "Shop" || !file.Set("landing") || file.Set("checks_timeout") {
		t.Fatalf("load: %+v %v", file.Project, err)
	}
	if _, err := s.Current(pidB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("project without settings: %v", err)
	}
}

func TestAtReadsAnEarlierRevision(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	s := r.store()
	first, _ := s.Current(pidA)
	if _, err := s.Replace(pidA, first.Pin, []byte(settingsOf("a")+"# edited\n")); err != nil {
		t.Fatal(err)
	}
	got, err := s.At(pidA, 1)
	if err != nil || got.Pin != first.Pin || !bytes.Equal(got.Data, first.Data) {
		t.Fatalf("revision 1: %+v %v", got, err)
	}
	if _, err := s.At(pidA, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revision that does not exist: %v", err)
	}
}

func TestApplyLoadsTheRegistryInOrderWithTheCurrentSettings(t *testing.T) {
	r := newRoot(t)
	r.register(pidC, settingsOf("c"))
	r.register(pidA, settingsOf("a"))
	r.register(pidB, "version = 1\nname = \"B\"\nupstream = \"upstream/b\"\nclone = \"/tmp/never-here-b\"\nlanding = \"squash\"\ndelivery = \"jev\"\nflake_fixes = true\n")
	s := r.store()
	current, _ := s.Current(pidA)
	if _, err := s.Replace(pidA, current.Pin, []byte(strings.Replace(settingsOf("a"), "checks_timeout", "base_branch = \"develop\"\nchecks_timeout", 1))); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.Apply(r.config(), r.home)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectIDs(); !slices.Equal(got, []config.ProjectID{pidC, pidA, pidB}) || !slices.Equal(cfg.ActiveProjects, []string{pidC, pidA, pidB}) {
		t.Fatalf("projects %v, active %v", got, cfg.ActiveProjects)
	}
	a, b := cfg.For(pidA).Project, cfg.For(pidB).Project
	if a.BaseBranch != "develop" || a.ChecksTimeout != "30m" || a.Upstream != "upstream/a" {
		t.Fatalf("project a does not hold its newest settings: %+v", a)
	}
	if b.Name != "B" || b.Landing != "squash" || b.Delivery != config.DeliveryJev || !b.FlakeFixes {
		t.Fatalf("project b: %+v", b)
	}
	if cfg.HasProject() {
		t.Fatal("several projects have no single project")
	}
}

func TestApplyOfOneProjectSetsTheConfigurationsProject(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	cfg, err := r.store().Apply(r.config(), r.home)
	if err != nil || !cfg.HasProject() || cfg.Project.ID != pidA || cfg.Project.Upstream != "upstream/a" {
		t.Fatalf("single project: %+v %v", cfg, err)
	}
}

func TestApplyOfAnEmptyRegistryHoldsNoProject(t *testing.T) {
	r := newRoot(t)
	cfg, err := r.store().Apply(r.config(), r.home)
	if err != nil || len(cfg.Projects) != 0 || cfg.HasProject() {
		t.Fatalf("apply: %+v %v", cfg, err)
	}
}

func TestApplyLeavesTheGivenConfigurationUnchanged(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	top := r.config()
	if _, err := r.store().Apply(top, r.home); err != nil {
		t.Fatal(err)
	}
	if len(top.Projects) != 0 || top.HasProject() {
		t.Fatalf("the receiver changed: %+v", top)
	}
}

func TestApplyNamesTheProjectWhoseSettingsDoNotLoad(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	r.register(pidB, strings.Replace(settingsOf("b"), `fork = "owner/b"`, `fork = "not a repository"`, 1))
	r.register(pidC, settingsOf("c"))
	_, err := r.store().Apply(r.config(), r.home)
	var loadErr *LoadError
	var field *config.FieldError
	if !errors.As(err, &loadErr) || loadErr.Project != pidB || !errors.As(err, &field) || field.Field != "fork" {
		t.Fatalf("apply: %v", err)
	}
	if strings.Contains(err.Error(), "not a repository") || strings.Contains(err.Error(), "config.toml") {
		t.Fatalf("the error quotes a value or names a file: %v", err)
	}
	if !strings.Contains(err.Error(), "osmia project settings "+pidB+" set fork=VALUE") {
		t.Fatalf("the error does not say how to repair the settings: %v", err)
	}
}

func TestLoadConfigAppliesTheRegistryOfTheRoot(t *testing.T) {
	r := newRoot(t)
	r.register(pidB, settingsOf("b"))
	r.register(pidA, settingsOf("a"))
	cfg, err := LoadConfig(r.opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectIDs(); !slices.Equal(got, []config.ProjectID{pidB, pidA}) {
		t.Fatalf("projects %v", got)
	}
	if cfg.Listen.Socket == "" || cfg.Root.String() != r.dir {
		t.Fatalf("top-level settings are missing: %+v", cfg.Listen)
	}
}

func TestLoadConfigRefusesAnInvalidTopLevelFile(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	if err := os.WriteFile(filepath.Join(r.dir, "config.toml"), []byte("version = 1\nactive_projects = []\n[profiles.default]\nagent = \"claude\"\nmodel = \"x\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(r.opts)
	var field *config.FieldError
	if cfg != nil || !errors.As(err, &field) || field.Field != "active_projects" || field.Reason != "unknown configuration key" {
		t.Fatalf("load: %+v %v", cfg, err)
	}
}

func TestRelabelNamesTheProjectInPlaceOfItsFile(t *testing.T) {
	r := newRoot(t)
	cfg := r.config()
	_, err := cfg.DecodeProject(pidA, r.home, []byte("version = 1\nupstream = \"nope\"\nclone = \"~/x\"\n"))
	var before *config.FieldError
	if !errors.As(err, &before) {
		t.Fatalf("decode: %v", err)
	}
	var after *config.FieldError
	relabeled := Relabel(cfg.Root, pidA, err)
	if !errors.As(relabeled, &after) || after.Path != config.ProjectSettingsLabel(pidA) || after.Field != before.Field || after.Reason != before.Reason {
		t.Fatalf("relabel: %v", relabeled)
	}
	other := errors.New("not a field error")
	if Relabel(cfg.Root, pidA, other) != other || Relabel(cfg.Root, pidA, nil) != nil {
		t.Fatal("an error that is not about the settings changed")
	}
}

func TestAStepFailureStopsAWriteLikeACrash(t *testing.T) {
	crash := errors.New("crash")
	for _, step := range []string{"registry-staged", "revision-staged:" + pidA + ":1", "revision-recorded:" + pidA + ":1"} {
		t.Run(step, func(t *testing.T) {
			r := newRoot(t)
			interrupted := r.store(WithStep(func(name string) error {
				if name == step {
					return crash
				}
				return nil
			}))
			var err error
			if strings.HasPrefix(step, "registry") {
				err = interrupted.Add(pidA)
			} else {
				_, err = interrupted.WriteFirst(pidA, []byte(settingsOf("a")))
			}
			if !errors.Is(err, crash) {
				t.Fatalf("interrupted write: %v", err)
			}
			if strings.HasPrefix(step, "registry") && len(ids(t, r.store())) != 0 {
				t.Fatal("an interrupted registry write is visible")
			}
			s := r.store()
			if _, err := s.WriteFirst(pidA, []byte(settingsOf("a"))); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if err := s.Add(pidA); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if got := revisions(t, s, pidA); len(got) != 1 {
				t.Fatalf("%d revisions after resume", len(got))
			}
			filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, err error) error {
				if err == nil && strings.HasPrefix(d.Name(), tempPrefix) {
					t.Errorf("temporary file %s left behind", path)
				}
				return nil
			})
		})
	}
}

func TestNoOperationWritesTheTopLevelFile(t *testing.T) {
	r := newRoot(t)
	top := filepath.Join(r.dir, "config.toml")
	if err := os.Chmod(top, 0400); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, r.dir)["config.toml"]
	cfg := r.config()
	s := Open(cfg.Root)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.WriteFirst(pidA, []byte(settingsOf("a")))
	must(err)
	must(s.Add(pidA))
	must(s.Add(pidB))
	must(s.Remove(pidB))
	current, err := s.Current(pidA)
	must(err)
	_, _, err = s.Save(cfg, r.home, pidA, current.Pin, config.ProjectEdit{Strings: map[string]string{"landing": "squash"}})
	must(err)
	_, err = s.Apply(cfg, r.home)
	must(err)
	if got := snapshot(t, r.dir)["config.toml"]; got != before {
		t.Fatal("the top-level file changed")
	}
	must(os.Remove(top))
	must(s.Add(pidD))
	if _, err := os.Lstat(top); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the top-level file was created")
	}
}

func TestStoreAddsNothingToProjectDirectories(t *testing.T) {
	r := newRoot(t)
	projectDir := filepath.Join(r.dir, "projects", pidA)
	if err := os.MkdirAll(projectDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"trace.db", "osmia.lock"} {
		if err := os.WriteFile(filepath.Join(projectDir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r.register(pidA, settingsOf("a"))
	s := r.store()
	current, _ := s.Current(pidA)
	if _, err := s.Replace(pidA, current.Pin, []byte(settingsOf("a"))); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"osmia.lock", "trace.db"}) {
		t.Fatalf("project directory holds %v", names)
	}
	top, err := os.ReadDir(r.dir)
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

func TestConcurrentWritersRecordOneFirstRevisionAndEveryRegistration(t *testing.T) {
	r := newRoot(t)
	want := []string{pidA, pidB, pidC, pidD}
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := r.store()
			for _, id := range want {
				if _, err := s.WriteFirst(config.ProjectID(id), []byte(settingsOf(id[2:3]))); err != nil {
					errs[i] = err
					return
				}
				if err := s.Add(config.ProjectID(id)); err != nil {
					errs[i] = err
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	s := r.store()
	registered := ids(t, s)
	slices.Sort(registered)
	if !slices.Equal(registered, want) {
		t.Fatalf("registry %v, want every project once: %v", registered, want)
	}
	for _, id := range want {
		if got := revisions(t, s, id); len(got) != 1 {
			t.Fatalf("%s: %d first revisions", id, len(got))
		}
	}
}

func TestAddAndRemoveRefuseAnIDThatIsNotAProjectIdentity(t *testing.T) {
	r := newRoot(t)
	r.register(pidA, settingsOf("a"))
	before := snapshot(t, r.dir)
	for _, id := range []string{"../escape", "p_short", "w_" + pidA[2:]} {
		if err := r.store().Add(config.ProjectID(id)); err == nil {
			t.Fatalf("added %q", id)
		}
		if err := r.store().Remove(config.ProjectID(id)); err == nil {
			t.Fatalf("removed %q", id)
		}
	}
	if !reflect.DeepEqual(before, snapshot(t, r.dir)) {
		t.Fatal("a refused registration wrote something")
	}
}

func TestRegistryHoldingADuplicateOrMalformedIDDoesNotLoad(t *testing.T) {
	for name, projects := range map[string]string{
		"duplicate": `["` + pidA + `", "` + pidA + `"]`,
		"traversal": `["../escape"]`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRoot(t)
			dir := filepath.Join(r.dir, "project-store")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			data := `{"version": 1, "projects": ` + projects + `}`
			if err := os.WriteFile(filepath.Join(dir, "registry.json"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := r.store().Projects(); err == nil {
				t.Fatal("the registry loaded")
			}
			if _, err := r.store().Apply(r.config(), r.home); err == nil {
				t.Fatal("apply loaded the registry")
			}
		})
	}
}
