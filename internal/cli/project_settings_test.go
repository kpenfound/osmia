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

// settingsRoot is a root whose project registry and settings hold one project,
// imported from a legacy root the way the service imports it. The project's
// retired file is replaced by a directory, so any read of it fails.
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
	writeFile(t, filepath.Join(r.root, "config.toml"), "# owned elsewhere\nversion = 1\nactive_projects = [\""+project+"\"]\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n")
	writeFile(t, filepath.Join(r.root, "projects", project, "config.toml"), "# the project's own comment\nversion = 1\nupstream = \"upstream/repo\"\nfork = \"owner/repo\"\nclone = \""+r.clone+"\"\n")
	cfg, err := config.LoadTopLevel(config.Options{Root: r.root})
	must(t, err)
	r.store = projectstore.Open(cfg.Root)
	must(t, r.store.Import(context.Background(), cfg, ""))
	return r
}

func (r *settingsRoot) topLevel() string { return filepath.Join(r.root, "config.toml") }

// unreadableRetiredFile makes the project's retired file unreadable: a read of
// it fails.
func (r *settingsRoot) unreadableRetiredFile() {
	path := filepath.Join(r.root, "projects", project, "config.toml")
	must(r.t, os.Remove(path))
	must(r.t, os.Mkdir(path, 0700))
}

// breakClone turns the project's clone path into a regular file, so the
// stored settings no longer load.
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

func TestProjectSettingsRepairsSettingsThatDoNotLoad(t *testing.T) {
	r := newSettingsRoot(t)
	r.unreadableRetiredFile()
	r.breakClone()
	must(t, os.Chmod(r.topLevel(), 0400))
	if r.loadErr() == nil {
		t.Fatal("the stored settings still load")
	}
	before := snapshot(t, r.root)
	registry, err := os.ReadFile(filepath.Join(r.root, "project-store", "registry.json"))
	must(t, err)
	topLevel, err := os.ReadFile(r.topLevel())
	must(t, err)

	code, out, diag := r.run("show")
	if code != 0 || diag != "" || !strings.Contains(out, "do not load") || !strings.Contains(out, "clone: must be a directory") || !strings.Contains(out, "upstream/repo") {
		t.Fatalf("show: %d\n%s\n%s", code, out, diag)
	}
	if !reflect.DeepEqual(before, snapshot(t, r.root)) {
		t.Fatal("show changed the root")
	}

	// An edit that leaves the clone broken is refused, naming the field.
	code, out, diag = r.run("set", "base_branch=trunk")
	if code == 0 || out != "" || !strings.Contains(diag, "clone: must be a directory") || !strings.Contains(diag, "nothing was written") {
		t.Fatalf("edit leaving the clone broken: %d\n%s\n%s", code, out, diag)
	}
	if !reflect.DeepEqual(before, snapshot(t, r.root)) {
		t.Fatal("a refused edit changed the root")
	}

	good := filepath.Join(filepath.Dir(r.clone), "good")
	must(t, os.Mkdir(good, 0700))
	code, out, diag = r.run("set", "clone="+good)
	if code != 0 || diag != "" || !strings.Contains(out, "revision 2") || !strings.Contains(out, "replaces revision 1") {
		t.Fatalf("repair: %d\n%s\n%s", code, out, diag)
	}
	if err := r.loadErr(); err != nil {
		t.Fatalf("the repaired settings do not load: %v", err)
	}
	cfg, err := config.LoadTopLevel(config.Options{Root: r.root})
	must(t, err)
	file, revision, err := r.store.Load(cfg, "", config.ProjectID(project))
	must(t, err)
	if revision.Number != 2 || file.Project.Clone != good || file.Project.Upstream != "upstream/repo" || file.Project.Fork != "owner/repo" {
		t.Fatalf("repaired project %+v at revision %d", file.Project, revision.Number)
	}
	if !strings.Contains(string(revision.Data), "# the project's own comment") {
		t.Fatalf("the edit rewrote more than the clone:\n%s", revision.Data)
	}
	if r.revisions() != 2 {
		t.Fatalf("%d revisions, want the original and the corrected one", r.revisions())
	}
	after, err := os.ReadFile(filepath.Join(r.root, "project-store", "registry.json"))
	must(t, err)
	if string(after) != string(registry) {
		t.Fatalf("the registry changed:\n%s\n%s", registry, after)
	}
	if now, err := os.ReadFile(r.topLevel()); err != nil || string(now) != string(topLevel) {
		t.Fatalf("the top-level file changed: %v\n%s", err, now)
	}
	if info, err := os.Stat(filepath.Join(r.root, "projects", project, "config.toml")); err != nil || !info.IsDir() {
		t.Fatal("the retired file was touched")
	}

	code, out, diag = r.run("show")
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
	// A settings save of the same edit is refused with this text, which names the
	// project's settings rather than a file.
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

func TestProjectSettingsEditsAndClearsSettings(t *testing.T) {
	r := newSettingsRoot(t)
	code, out, diag := r.run("set", "base_branch=trunk", "capacity.git_operations=5", "flake_fixes=true", "hearsay_entities.local=code:owner/repo:main", "--json")
	var saved projectSettingsSaved
	if code != 0 || diag != "" || json.Unmarshal([]byte(out), &saved) != nil || saved.Project != project || saved.Revision != 2 || saved.Previous != 1 || saved.Pin == "" {
		t.Fatalf("set: %d\n%s\n%s", code, out, diag)
	}
	current, err := r.store.Current(config.ProjectID(project))
	must(t, err)
	if current.Pin != saved.Pin {
		t.Fatalf("pin %s, stored %s", saved.Pin, current.Pin)
	}
	var view projectSettingsView
	code, out, diag = r.run("show", "--json")
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

	successfulSettings := func(args ...string) {
		t.Helper()
		if code, out, diag := r.run(args...); code != 0 || diag != "" {
			t.Fatalf("%v: %d\n%s\n%s", args, code, out, diag)
		}
	}
	successfulSettings("clear", "base_branch", "hearsay_entities.local", "capacity.git_operations")
	successfulSettings("set", "fork=")
	cfg, err := config.LoadTopLevel(config.Options{Root: r.root})
	must(t, err)
	file, revision, err := r.store.Load(cfg, "", config.ProjectID(project))
	must(t, err)
	p := file.Project
	if revision.Number != 4 || p.BaseBranch != "main" || len(p.HearsayEntities) != 0 || p.Capacity.GitOperations != config.DefaultGitOperations || p.Fork != "" || !p.FlakeFixes {
		t.Fatalf("project %+v at revision %d\n%s", p, revision.Number, revision.Data)
	}
	if file.Set("base_branch") || file.Set("fork") || !file.Set("flake_fixes") {
		t.Fatalf("settings left set:\n%s", revision.Data)
	}
}

func TestProjectSettingsRefusesWhileTheServiceRuns(t *testing.T) {
	opts := fixture(t)
	root := opts.Config.Root
	cfg, err := config.LoadTopLevel(config.Options{Root: root})
	must(t, err)
	store := projectstore.Open(cfg.Root)
	must(t, store.Import(context.Background(), cfg, ""))
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
	// Once the service stops, the same command works.
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
			// Released, the command goes through.
			must(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN))
			if code, out, diag := r.run("set", "base_branch=trunk"); code != 0 {
				t.Fatalf("after the lock was released: %d\n%s\n%s", code, out, diag)
			}
		})
	}
}

func TestProjectSettingsHoldsTheServiceLockWhileItWrites(t *testing.T) {
	r := newSettingsRoot(t)
	// While the command holds the root, a service cannot take it; the lock is
	// free again after the command returns.
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
	// The other project has a file of its own that a legacy Osmia would read,
	// made unreadable: the command must not read it.
	must(t, os.MkdirAll(filepath.Join(r.root, "projects", other, "config.toml"), 0700))
	r.unreadableRetiredFile()
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
	// The registered project is not affected by the refusals.
	if code, out, diag := r.run("show"); code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, diag)
	}
}

func TestProjectSettingsRefusesARootThatIsNotImported(t *testing.T) {
	base := shortDir(t)
	root := filepath.Join(base, "r")
	writeFile(t, filepath.Join(root, "config.toml"), "version = 1\nactive_projects = [\""+project+"\"]\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n")
	// The legacy project file is the only record of the project; it must not be
	// read, and no store is created.
	writeFile(t, filepath.Join(root, "projects", project, "config.toml"), "version = 1\nupstream = \"a/b\"\nclone = \""+base+"/clone\"\n")
	before := snapshot(t, root)
	code, out, diag := invoke(t, root, "project", "settings", project, "set", "base_branch=trunk")
	if code == 0 || out != "" || !strings.Contains(diag, "not in the registry") {
		t.Fatalf("%d\n%s\n%s", code, out, diag)
	}
	if !reflect.DeepEqual(before, snapshot(t, root)) {
		t.Fatal("a refused command changed the root")
	}
}

func TestProjectSettingsIsOffline(t *testing.T) {
	// A root that does not exist stays uncreated, and no socket is involved.
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
