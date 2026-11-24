package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/service"
)

type settingsRoot struct {
	t     *testing.T
	root  string
	clone string
	store *projectstore.Store
}

func newSettingsRoot(t *testing.T) *settingsRoot {
	t.Helper()
	base := shortDir(t)
	r := &settingsRoot{t: t, root: filepath.Join(base, "r"), clone: filepath.Join(base, "clone")}
	must(t, os.Mkdir(r.clone, 0700))
	writeFile(t, filepath.Join(r.root, "config.toml"), "# owned elsewhere\nversion = 1\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n")
	register(t, r.root, project, "# the project's own comment\nversion = 1\nupstream = \"upstream/repo\"\nfork = \"owner/repo\"\nclone = \""+r.clone+"\"\n")
	cfg, err := config.LoadTopLevel(config.Options{Root: r.root})
	must(t, err)
	r.store = projectstore.Open(cfg.Root)
	return r
}

func (r *settingsRoot) topLevel() string { return filepath.Join(r.root, "config.toml") }

func (r *settingsRoot) breakClone() {
	must(r.t, os.Remove(r.clone))
	must(r.t, os.WriteFile(r.clone, []byte("not a directory"), 0600))
}

func (r *settingsRoot) loadErr() error {
	r.t.Helper()
	cfg, err := config.LoadTopLevel(config.Options{Root: r.root})
	must(r.t, err)
	_, _, err = r.store.Load(cfg, "", config.ProjectID(project))
	return err
}

func (r *settingsRoot) revisions() int {
	r.t.Helper()
	revisions, err := r.store.Revisions(config.ProjectID(project))
	must(r.t, err)
	return len(revisions)
}

func (r *settingsRoot) run(args ...string) (int, string, string) {
	return invoke(r.t, r.root, append([]string{"project", "settings", project}, args...)...)
}

type brokenSettings struct {
	*settingsRoot
	before       map[string]string
	registry     []byte
	topLevelData []byte
}

func newBrokenSettings(t *testing.T) brokenSettings {
	t.Helper()
	r := newSettingsRoot(t)
	r.breakClone()
	must(t, os.Chmod(r.topLevel(), 0400))
	if r.loadErr() == nil {
		t.Fatal("the stored settings still load")
	}
	registry, err := os.ReadFile(filepath.Join(r.root, "project-store", "registry.json"))
	must(t, err)
	topLevelData, err := os.ReadFile(r.topLevel())
	must(t, err)
	return brokenSettings{settingsRoot: r, before: snapshot(t, r.root), registry: registry, topLevelData: topLevelData}
}

func (b brokenSettings) repair(t *testing.T) string {
	t.Helper()
	good := filepath.Join(filepath.Dir(b.clone), "good")
	must(t, os.Mkdir(good, 0700))
	code, out, diag := b.run("set", "clone="+good)
	if code != 0 || diag != "" {
		t.Fatalf("repair: %d\n%s\n%s", code, out, diag)
	}
	return good
}

func TestProjectSettingsShowReportsSettingsThatDoNotLoad(t *testing.T) {
	b := newBrokenSettings(t)
	code, out, diag := b.run("show")
	if code != 0 || diag != "" || !strings.Contains(out, "do not load") || !strings.Contains(out, "clone: must be a directory") || !strings.Contains(out, "upstream/repo") {
		t.Fatalf("show: %d\n%s\n%s", code, out, diag)
	}
}

func TestProjectSettingsShowChangesNothingInTheRoot(t *testing.T) {
	b := newBrokenSettings(t)
	b.run("show")
	if !reflect.DeepEqual(b.before, snapshot(t, b.root)) {
		t.Fatal("show changed the root")
	}
}

func TestProjectSettingsRefusesAnEditThatLeavesTheCloneBroken(t *testing.T) {
	b := newBrokenSettings(t)
	code, out, diag := b.run("set", "base_branch=trunk")
	if code == 0 || out != "" || !strings.Contains(diag, "clone: must be a directory") || !strings.Contains(diag, "nothing was written") {
		t.Fatalf("edit leaving the clone broken: %d\n%s\n%s", code, out, diag)
	}
}

func TestProjectSettingsRefusedEditChangesNothingInTheRoot(t *testing.T) {
	b := newBrokenSettings(t)
	b.run("set", "base_branch=trunk")
	if !reflect.DeepEqual(b.before, snapshot(t, b.root)) {
		t.Fatal("a refused edit changed the root")
	}
}

func TestProjectSettingsRepairRecordsTheNextRevision(t *testing.T) {
	b := newBrokenSettings(t)
	good := filepath.Join(filepath.Dir(b.clone), "good")
	must(t, os.Mkdir(good, 0700))
	code, out, diag := b.run("set", "clone="+good)
	if code != 0 || diag != "" || !strings.Contains(out, "revision 2") || !strings.Contains(out, "replaces revision 1") {
		t.Fatalf("repair: %d\n%s\n%s", code, out, diag)
	}
	if r := b.revisions(); r != 2 {
		t.Fatalf("%d revisions, want the original and the corrected one", r)
	}
}

func TestProjectSettingsRepairedSettingsLoad(t *testing.T) {
	b := newBrokenSettings(t)
	good := b.repair(t)
	if err := b.loadErr(); err != nil {
		t.Fatalf("the repaired settings do not load: %v", err)
	}
	cfg, err := config.LoadTopLevel(config.Options{Root: b.root})
	must(t, err)
	file, revision, err := b.store.Load(cfg, "", config.ProjectID(project))
	must(t, err)
	if revision.Number != 2 || file.Project.Clone != good || file.Project.Upstream != "upstream/repo" || file.Project.Fork != "owner/repo" {
		t.Fatalf("repaired project %+v at revision %d", file.Project, revision.Number)
	}
}

func TestProjectSettingsRepairRewritesOnlyTheClone(t *testing.T) {
	b := newBrokenSettings(t)
	b.repair(t)
	cfg, err := config.LoadTopLevel(config.Options{Root: b.root})
	must(t, err)
	_, revision, err := b.store.Load(cfg, "", config.ProjectID(project))
	must(t, err)
	if !strings.Contains(string(revision.Data), "# the project's own comment") {
		t.Fatalf("the edit rewrote more than the clone:\n%s", revision.Data)
	}
}

func TestProjectSettingsRepairLeavesTheRegistryAndTopLevelFileAlone(t *testing.T) {
	b := newBrokenSettings(t)
	b.repair(t)
	after, err := os.ReadFile(filepath.Join(b.root, "project-store", "registry.json"))
	must(t, err)
	if string(after) != string(b.registry) {
		t.Fatalf("the registry changed:\n%s\n%s", b.registry, after)
	}
	if now, err := os.ReadFile(b.topLevel()); err != nil || string(now) != string(b.topLevelData) {
		t.Fatalf("the top-level file changed: %v\n%s", err, now)
	}
}

func TestProjectSettingsShowAfterTheRepairNamesTheNewClone(t *testing.T) {
	b := newBrokenSettings(t)
	good := b.repair(t)
	code, out, diag := b.run("show")
	if code != 0 || diag != "" || !strings.Contains(out, "revision 2") || !strings.Contains(out, `"`+good+`"`) || strings.Contains(out, "do not load") {
		t.Fatalf("show after the repair: %d\n%s\n%s", code, out, diag)
	}
}

func TestProjectSettingsRefusesAnInvalidEditNamingTheField(t *testing.T) {
	r := newSettingsRoot(t)
	cfg, err := config.LoadTopLevel(config.Options{Root: r.root})
	must(t, err)
	current, err := r.store.Current(config.ProjectID(project))
	must(t, err)
	_, _, saveErr := cfg.EditProject(config.ProjectID(project), "", current.Data, config.ProjectEdit{Strings: map[string]string{"checks_timeout": "soon"}})
	if saveErr == nil {
		t.Fatal("the edit is valid")
	}
	before := snapshot(t, r.root)
	for _, c := range []struct {
		args  []string
		field string
		text  string
	}{
		{[]string{"set", "checks_timeout=soon"}, "checks_timeout", projectstore.Relabel(cfg.Root, config.ProjectID(project), saveErr).Error()},
		{[]string{"set", "upstream=https://example.com/a/b"}, "upstream", "expected owner/repository"},
		{[]string{"set", "capacity.git_operations=0"}, "capacity.git_operations", "must be positive"},
		{[]string{"set", "capacity.git_operations=many"}, "capacity.git_operations", "must be a whole number"},
		{[]string{"set", "flake_fixes=maybe"}, "flake_fixes", "must be true or false"},
		{[]string{"set", "colour=blue"}, "colour", "is not a setting of a project"},
		{[]string{"set", "version=2"}, "version", "is not a setting of a project"},
		{[]string{"set", "hearsay_entities=x"}, "hearsay_entities", "is not a setting of a project"},
		{[]string{"set", "name"}, "name", "needs a value"},
		{[]string{"set", "name=a", "name=b"}, "name", "is changed more than once"},
		{[]string{"set", "upstream="}, "upstream", "expected owner/repository"},
		{[]string{"clear", "upstream"}, "upstream", "is required and has no default"},
		{[]string{"clear", "colour"}, "colour", "is not a setting that can be cleared"},
		{[]string{"clear", "name=x"}, "name", "is cleared by name alone"},
		{[]string{"set", "hearsay_entities.=x"}, "hearsay_entities", "must map local entity IDs"},
	} {
		code, out, diag := r.run(c.args...)
		if code == 0 || out != "" || !strings.Contains(diag, c.field+": ") || !strings.Contains(diag, c.text) || !strings.Contains(diag, "nothing was written") {
			t.Fatalf("%v: %d\n%s\n%s", c.args, code, out, diag)
		}
		if strings.Contains(diag, "config.toml") || !strings.Contains(diag, config.ProjectSettingsLabel(project)+": "+c.field) {
			t.Fatalf("%v: the refusal does not name the project's settings: %s", c.args, diag)
		}
		if strings.Contains(diag, "soon") && c.field != "checks_timeout" {
			t.Fatalf("%v: %s", c.args, diag)
		}
		if !reflect.DeepEqual(before, snapshot(t, r.root)) {
			t.Fatalf("%v: a refused edit changed the root", c.args)
		}
	}
	if r.revisions() != 1 {
		t.Fatalf("%d revisions after refused edits", r.revisions())
	}
}

func (r *settingsRoot) setSeveral(t *testing.T) projectSettingsSaved {
	t.Helper()
	code, out, diag := r.run("set", "base_branch=trunk", "capacity.git_operations=5", "flake_fixes=true", "hearsay_entities.local=code:owner/repo:main", "--json")
	var saved projectSettingsSaved
	if code != 0 || diag != "" || json.Unmarshal([]byte(out), &saved) != nil {
		t.Fatalf("set: %d\n%s\n%s", code, out, diag)
	}
	return saved
}

func TestProjectSettingsSetSavesTheNextRevision(t *testing.T) {
	r := newSettingsRoot(t)
	saved := r.setSeveral(t)
	if saved.Project != project || saved.Revision != 2 || saved.Previous != 1 || saved.Pin == "" {
		t.Fatalf("set: %+v", saved)
	}
}

func TestProjectSettingsSetReportsThePinOfTheStoredRevision(t *testing.T) {
	r := newSettingsRoot(t)
	saved := r.setSeveral(t)
	current, err := r.store.Current(config.ProjectID(project))
	must(t, err)
	if current.Pin != saved.Pin {
		t.Fatalf("pin %s, stored %s", saved.Pin, current.Pin)
	}
}

func TestProjectSettingsShowReportsTheSavedValuesAndDefaults(t *testing.T) {
	r := newSettingsRoot(t)
	saved := r.setSeveral(t)
	var view projectSettingsView
	code, out, diag := r.run("show", "--json")
	if code != 0 || diag != "" || json.Unmarshal([]byte(out), &view) != nil || view.Revision != 2 || view.Pin != saved.Pin || view.Error != "" {
		t.Fatalf("show: %d\n%s\n%s", code, out, diag)
	}
	values := map[string]projectSettingView{}
	for _, s := range view.Settings {
		values[s.Name] = s
	}
	if len(view.Settings) != len(config.ProjectSettings) ||
		values["base_branch"].Value != "trunk" || values["base_branch"].Default ||
		values["capacity.git_operations"].Value != float64(5) || values["flake_fixes"].Value != true ||
		!reflect.DeepEqual(values["hearsay_entities"].Value, map[string]any{"local": "code:owner/repo:main"}) ||
		!values["landing"].Default || values["upstream"].Default {
		t.Fatalf("settings %+v", view.Settings)
	}
}

func (r *settingsRoot) loadStored(t *testing.T) (config.ProjectFile, projectstore.Revision) {
	t.Helper()
	cfg, err := config.LoadTopLevel(config.Options{Root: r.root})
	must(t, err)
	file, revision, err := r.store.Load(cfg, "", config.ProjectID(project))
	must(t, err)
	return file, revision
}

func TestProjectSettingsClearRestoresTheDefaults(t *testing.T) {
	r := newSettingsRoot(t)
	r.setSeveral(t)
	if code, out, diag := r.run("clear", "base_branch", "hearsay_entities.local", "capacity.git_operations"); code != 0 || diag != "" {
		t.Fatalf("clear: %d\n%s\n%s", code, out, diag)
	}
	file, revision := r.loadStored(t)
	p := file.Project
	if revision.Number != 3 || p.BaseBranch != "main" || len(p.HearsayEntities) != 0 || p.Capacity.GitOperations != config.DefaultGitOperations || file.Set("base_branch") || !file.Set("flake_fixes") {
		t.Fatalf("project %+v at revision %d\n%s", p, revision.Number, revision.Data)
	}
}

func TestProjectSettingsSetWithAnEmptyValueUnsetsTheSetting(t *testing.T) {
	r := newSettingsRoot(t)
	if code, out, diag := r.run("set", "fork="); code != 0 || diag != "" {
		t.Fatalf("set: %d\n%s\n%s", code, out, diag)
	}
	file, revision := r.loadStored(t)
	if file.Project.Fork != "" || file.Set("fork") {
		t.Fatalf("fork left set at revision %d:\n%s", revision.Number, revision.Data)
	}
}

func TestProjectSettingsRefusesWhileTheServiceRuns(t *testing.T) {
	opts := fixture(t)
	root := opts.Config.Root
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })

	storeBefore := snapshot(t, filepath.Join(root, "project-store"))
	topLevel, err := os.ReadFile(filepath.Join(root, "config.toml"))
	must(t, err)
	for _, args := range [][]string{{"show"}, {"set", "base_branch=trunk"}, {"clear", "fork"}} {
		code, out, diag := invoke(t, root, append([]string{"project", "settings", project}, args...)...)
		if code == 0 || out != "" || !strings.Contains(diag, "running service") || !strings.Contains(diag, "Settings page") || !strings.Contains(diag, "/v1/projects/config/"+project) || !strings.Contains(diag, "nothing was changed") {
			t.Fatalf("%v: %d\n%s\n%s", args, code, out, diag)
		}
	}
	if !reflect.DeepEqual(storeBefore, snapshot(t, filepath.Join(root, "project-store"))) {
		t.Fatal("a refused command changed the store")
	}
	if now, err := os.ReadFile(filepath.Join(root, "config.toml")); err != nil || string(now) != string(topLevel) {
		t.Fatalf("the top-level file changed: %v", err)
	}
	s.Close()
	if code, out, diag := invoke(t, root, "project", "settings", project, "set", "base_branch=trunk"); code != 0 {
		t.Fatalf("after the service stopped: %d\n%s\n%s", code, out, diag)
	}
}

func TestProjectSettingsRefusesWhileALockIsHeld(t *testing.T) {
	for _, held := range []struct {
		name string
		path func(r *settingsRoot) string
	}{
		{"the root's ownership lock", func(r *settingsRoot) string { return filepath.Join(r.root, ".service.lock") }},
		{"the project's trace lock", func(r *settingsRoot) string { return filepath.Join(r.root, "projects", project, "osmia.lock") }},
	} {
		t.Run(held.name, func(t *testing.T) {
			r := newSettingsRoot(t)
			path := held.path(r)
			must(t, os.MkdirAll(filepath.Dir(path), 0700))
			lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
			must(t, err)
			defer lock.Close()
			must(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
			before := snapshot(t, r.root)
			for _, args := range [][]string{{"show"}, {"set", "base_branch=trunk"}} {
				code, out, diag := r.run(args...)
				if code == 0 || out != "" || !strings.Contains(diag, "Settings page") || !strings.Contains(diag, "nothing was changed") {
					t.Fatalf("%v: %d\n%s\n%s", args, code, out, diag)
				}
			}
			if !reflect.DeepEqual(before, snapshot(t, r.root)) {
				t.Fatal("a refused command changed the root")
			}
			must(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN))
			if code, out, diag := r.run("set", "base_branch=trunk"); code != 0 {
				t.Fatalf("after the lock was released: %d\n%s\n%s", code, out, diag)
			}
		})
	}
}

func TestProjectSettingsHoldsTheServiceLockWhileItWrites(t *testing.T) {
	r := newSettingsRoot(t)
	if code, out, diag := r.run("set", "base_branch=trunk"); code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, diag)
	}
	lock, err := os.OpenFile(filepath.Join(r.root, ".service.lock"), os.O_RDWR, 0600)
	must(t, err)
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the command left the root locked: %v", err)
	}
}

func TestProjectSettingsRefusesAProjectNotInTheRegistry(t *testing.T) {
	const other = "p_fedcba9876543210fedcba9876543210"
	r := newSettingsRoot(t)
	before := snapshot(t, r.root)
	for _, args := range [][]string{{"show"}, {"set", "base_branch=trunk"}} {
		code, out, diag := invoke(t, r.root, append([]string{"project", "settings", other}, args...)...)
		if code == 0 || out != "" || !strings.Contains(diag, other) || !strings.Contains(diag, "not in the registry") {
			t.Fatalf("%v: %d\n%s\n%s", args, code, out, diag)
		}
	}
	if !reflect.DeepEqual(before, snapshot(t, r.root)) {
		t.Fatal("a refused command changed the root")
	}
	if code, out, diag := r.run("show"); code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, diag)
	}
}

func TestProjectSettingsIsOffline(t *testing.T) {
	root := filepath.Join(shortDir(t), "missing")
	code, out, diag := invoke(t, root, "project", "settings", project, "show")
	if code == 0 || out != "" || diag == "" {
		t.Fatalf("%d\n%s\n%s", code, out, diag)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("the root was created: %v", err)
	}
	r := newSettingsRoot(t)
	if code, out, diag := r.run("set", "base_branch=trunk"); code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, diag)
	}
	if _, err := os.Lstat(filepath.Join(r.root, "osmia.sock")); !os.IsNotExist(err) {
		t.Fatalf("a socket exists: %v", err)
	}
}

func TestProjectSettingsUsage(t *testing.T) {
	root := newSettingsRoot(t).root
	for _, args := range [][]string{
		{"project", "settings"}, {"project", "settings", project}, {"project", "settings", "not-an-id", "show"},
		{"project", "settings", project, "show", "extra"}, {"project", "settings", project, "set"}, {"project", "settings", project, "clear"},
		{"project", "settings", project, "edit", "name=a"}, {"project", "settings", project, "show", "--socket", "x.sock"},
		{"project", "settings", project, "show", "--clone", "x"},
	} {
		code, out, diag := invoke(t, root, args...)
		if code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: %d %s %s", args, code, out, diag)
		}
	}
	code, out, _ := invoke(t, root, "--help")
	if code != 0 || !strings.Contains(out, "project settings <project-id> show") || !strings.Contains(out, "project settings <project-id> set NAME=VALUE") || !strings.Contains(out, "project settings <project-id> clear NAME") {
		t.Fatalf("usage does not list project settings: %d %q", code, out)
	}
}
