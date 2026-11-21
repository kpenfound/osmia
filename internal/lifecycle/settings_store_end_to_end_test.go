package lifecycle_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/cli"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/servicetest"
)

// fileState is a file's bytes and modification time, so a test can show a file
// was neither rewritten nor touched.
type fileState struct {
	data    string
	modTime time.Time
}

func stateOf(t *testing.T, path string) fileState {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	info, err := os.Stat(path)
	must(t, err)
	return fileState{string(data), info.ModTime()}
}

// osmia runs an osmia command against root and returns its exit code and
// output.
func osmia(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var out, diag bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"--root", root}, args...), strings.NewReader(""), &out, &diag)
	return code, out.String(), diag.String()
}

// The whole path, from a legacy root whose top-level file is read-only and
// carries active_projects and a project file: startup imports them, settings are
// read and saved, a save the service cannot adopt stays pending across a reload
// of the top-level file, projects are added, created and removed, an add is
// interrupted, the service restarts, stored settings stop loading, startup
// refuses, the repair command corrects them and the service starts again. The
// top-level file stays byte-identical and untouched throughout, and no project
// file is written after the import retired it.
//
// Serial: the project creation needs GITHUB_TOKEN.
func TestSettingsStoreRunsTheWholePathFromALegacyRoot(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	ctx := context.Background()
	opts := fixture(t)
	root := opts.Config.Root
	home := filepath.Dir(root)
	top := filepath.Join(root, "config.toml")
	retiredPath := filepath.Join(root, "projects", string(project), "config.toml")

	// Project creation, with a fake GitHub whose repository is a local bare one.
	cloneDir := filepath.Join(home, "clones")
	remote, _ := servicetest.SeedRemote(t, home, "newrepo", "main")
	github := servicetest.NewFakeGitHub(t, "owner")
	github.Remote = func(string) string { return remote }
	opts.Repositories = github.Client()
	legacy, err := os.ReadFile(top)
	must(t, err)
	must(t, os.WriteFile(top, append(legacy, fmt.Sprintf("\n[projects]\nrepo_owner = \"owner\"\nclone_dir = %q\n", cloneDir)...), 0400))
	must(t, os.Chmod(top, 0400))
	baseline := stateOf(t, top)
	untouched := func(step string) {
		t.Helper()
		if got := stateOf(t, top); got != baseline {
			t.Fatalf("%s: the top-level file changed", step)
		}
	}

	// Startup imports the list and the project's file.
	s, c := start(t, opts)
	untouched("startup")
	store := projectstore.Open(mustRoot(t, root))
	if done, err := store.Imported(); err != nil || !done {
		t.Fatalf("import complete: %t %v", done, err)
	}
	if got := activeProjects(t, root); !reflect.DeepEqual(got, []string{string(project)}) {
		t.Fatalf("registry after the import: %v", got)
	}
	if got := stateOf(t, retiredPath); !strings.HasPrefix(got.data, projectstore.RetiredMark) {
		t.Fatalf("the imported file is not retired: %q", got.data)
	}

	// Read and save settings.
	first, err := c.ProjectConfig(ctx, project)
	must(t, err)
	if first.Pin == "" || first.Revision != 1 || first.Error != nil {
		t.Fatalf("first read: %+v", first)
	}
	saved, err := c.SaveProjectConfig(ctx, project, wire.ProjectConfigEdit{Pin: first.Pin, Set: wire.ProjectConfigValues{ChecksTimeout: textOf("45m")}})
	must(t, err)
	if saved.Outcome != wire.ProjectConfigApplied || saved.Pin == first.Pin || saved.Revision != 2 || s.current().For(project).Project.ChecksTimeout != "45m" {
		t.Fatalf("applied save: %+v", saved)
	}
	// A stale pin is a conflict and writes nothing.
	_, err = c.SaveProjectConfig(ctx, project, wire.ProjectConfigEdit{Pin: first.Pin, Set: wire.ProjectConfigValues{Landing: textOf("squash")}})
	assertCode(t, err, Conflict)
	if got := storedSettings(t, root, project); !strings.Contains(got, `checks_timeout = "45m"`) || strings.Contains(got, "squash") {
		t.Fatalf("a stale save changed the stored settings:\n%s", got)
	}
	// An invalid edit is refused naming the field, and not a file.
	_, err = c.SaveProjectConfig(ctx, project, wire.ProjectConfigEdit{Pin: saved.Pin, Set: wire.ProjectConfigValues{ChecksTimeout: textOf("soon")}})
	assertCode(t, err, Validation)
	if !strings.Contains(err.Error(), config.ProjectSettingsLabel(project)+": checks_timeout: ") || strings.Contains(err.Error(), "config.toml") {
		t.Fatalf("invalid edit: %v", err)
	}
	// A failure after the write is saved but not applied.
	s.boundary = func(name string) error {
		if name == "project-config-written" {
			return errors.New("the scheduler is unreachable")
		}
		return nil
	}
	pending, err := c.SaveProjectConfig(ctx, project, wire.ProjectConfigEdit{Pin: saved.Pin, Set: wire.ProjectConfigValues{Landing: textOf("squash")}})
	must(t, err)
	s.boundary = nil
	if pending.Outcome != wire.ProjectConfigSavedNotApplied || !strings.Contains(pending.Reason, "the scheduler is unreachable") || s.current().For(project).Project.Landing != "commit-per-unit" {
		t.Fatalf("not applied save: %+v", pending)
	}
	untouched("settings saves")

	// With the save pending, a top-level change and a reload apply the top-level
	// change only. Editing the retired file and the leftover list changes nothing.
	edited, err := os.ReadFile(top)
	must(t, err)
	relisted := strings.Replace(string(edited), `"`+string(project)+`"`, `"p_00000000000000000000000000000007"`, 1)
	if relisted == string(edited) {
		t.Fatal("the leftover list is not in the top-level file")
	}
	must(t, os.Chmod(top, 0600))
	must(t, os.WriteFile(top, []byte(relisted+"\n[capacity]\nmasons = 7\n"), 0600))
	must(t, os.Chmod(top, 0400))
	baseline = stateOf(t, top)
	must(t, os.WriteFile(retiredPath, []byte("landing = \"sideways\"\n"), 0600))
	retired := stateOf(t, retiredPath)
	reloaded, err := c.Reload(ctx)
	must(t, err)
	now, err := c.Configuration(ctx)
	must(t, err)
	if reloaded.Digest != now.Digest || now.Effective.Capacity.Masons != 7 || len(now.Effective.Projects) != 1 || now.Effective.Projects[0].Landing != "commit-per-unit" || now.Effective.Projects[0].ChecksTimeout != "45m" {
		t.Fatalf("after the reload: %+v", now.Effective.Projects)
	}
	read, err := c.ProjectConfig(ctx, project)
	must(t, err)
	if read.Pin != pending.Pin || !settingNamed(t, read, "landing").Pending || settingNamed(t, read, "landing").Value != "squash" {
		t.Fatalf("the reload did not leave the saved revision pending: %+v", read)
	}
	if now.Drift.Differs || len(now.Drift.Files) != 1 || now.Drift.Files[0].Path != top {
		t.Fatalf("drift: %+v", now.Drift)
	}
	if got := stateOf(t, retiredPath); got != retired {
		t.Fatal("the retired file changed")
	}
	untouched("reload")

	// Add, create and remove change the registry and write no project file.
	cloneA := filepath.Join(home, "clone-a")
	demoGit(t, home, "init", "--quiet", cloneA)
	addedA, err := c.AddProject(ctx, request(cloneA))
	must(t, err)
	req := wire.ProjectCreateRequest{Name: "New Project", Repo: "newrepo", Description: "A new project", Charter: creationCharter}
	createdB, err := c.CreateProject(ctx, req)
	must(t, err)
	if got := activeProjects(t, root); !reflect.DeepEqual(got, []string{string(project), string(addedA.Project.ID), string(createdB.Project.ID)}) {
		t.Fatalf("registry after add and create: %v", got)
	}
	for _, id := range []config.ProjectID{addedA.Project.ID, createdB.Project.ID} {
		if _, err := os.Lstat(filepath.Join(root, "projects", string(id), "config.toml")); !os.IsNotExist(err) {
			t.Fatalf("project %s has a project file: %v", id, err)
		}
		if got := storedSettings(t, root, id); !strings.HasPrefix(got, "version = 1\n") {
			t.Fatalf("project %s stored settings: %q", id, got)
		}
	}
	_, err = c.RemoveProject(ctx, addedA.Project.ID)
	must(t, err)
	if got := activeProjects(t, root); !reflect.DeepEqual(got, []string{string(project), string(createdB.Project.ID)}) {
		t.Fatalf("registry after the removal: %v", got)
	}
	untouched("add, create and remove")

	// An add interrupted after its trace was created finishes on the next start
	// with the same identity and one trace.
	cloneC := filepath.Join(home, "clone-c")
	demoGit(t, home, "init", "--quiet", cloneC)
	s.boundary = func(name string) error {
		if name == "trace-created" {
			return errors.New("crash")
		}
		return nil
	}
	_, err = c.AddProject(ctx, request(cloneC))
	assertCode(t, err, Internal)
	s.boundary = nil
	interrupted := interruptedProject(t, root, project, addedA.Project.ID, createdB.Project.ID)

	// Restart: the interrupted add finishes, and the projects and settings
	// survive in registry order.
	must(t, s.Close())
	s, c = start(t, opts)
	want := []string{string(project), string(createdB.Project.ID), string(interrupted)}
	if got := activeProjects(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("registry after the restart: %v, want %v", got, want)
	}
	now, err = c.Configuration(ctx)
	must(t, err)
	if !reflect.DeepEqual(now.Effective.ActiveProjects, want) {
		t.Fatalf("effective.active_projects %v, want %v", now.Effective.ActiveProjects, want)
	}
	if p := s.current().For(project).Project; p.ChecksTimeout != "45m" || p.Landing != "squash" {
		t.Fatalf("the last saved settings after the restart: %+v", p)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "projects")); err != nil || len(entries) != 4 {
		t.Fatalf("project directories: %v %v", entries, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "project-add.json")); !os.IsNotExist(err) {
		t.Fatalf("the journal remains: %v", err)
	}
	code, text, diag := osmia(t, root, "config")
	if code != 0 || diag != "" {
		t.Fatalf("osmia config: %d %s %s", code, text, diag)
	}
	var shown []string
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "Registered project: "); ok {
			shown = append(shown, strings.Fields(rest)[0])
		}
	}
	if !reflect.DeepEqual(shown, want) {
		t.Fatalf("osmia config shows %v, want %v\n%s", shown, want, text)
	}
	untouched("restart")
	cloneOfProject := s.current().For(project).Project.Clone

	// Stored settings that stop loading refuse startup, naming the project, the
	// field and the reason and pointing to the repair command.
	must(t, s.Close())
	must(t, os.RemoveAll(cloneOfProject))
	must(t, os.WriteFile(cloneOfProject, []byte("not a directory"), 0600))
	_, err = Start(ctx, opts)
	if err == nil {
		t.Fatal("startup did not refuse stored settings that do not load")
	}
	for _, want := range []string{config.ProjectSettingsLabel(project), "clone", "must be a directory", "osmia project settings " + string(project) + " set clone=VALUE"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal lacks %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "config.toml") || strings.Contains(err.Error(), "by hand") {
		t.Fatalf("the refusal points at a file: %v", err)
	}
	// The command refuses nothing while the service is stopped, and repairs the
	// stored settings without reading the retired file.
	repaired := filepath.Join(home, "repaired-clone")
	demoGit(t, home, "init", "--quiet", repaired)
	must(t, os.Remove(retiredPath))
	must(t, os.Mkdir(retiredPath, 0700))
	code, text, diag = osmia(t, root, "project", "settings", string(project), "set", "clone="+repaired)
	if code != 0 {
		t.Fatalf("repair: %d %s %s", code, text, diag)
	}
	must(t, os.Remove(retiredPath))
	must(t, os.WriteFile(retiredPath, []byte(retired.data), 0600))
	untouched("the refused start and the repair")
	s, c = start(t, opts)
	if got := activeProjects(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("registry after the repair: %v, want %v", got, want)
	}
	if p := s.current().For(project).Project; p.Clone != repaired || p.ChecksTimeout != "45m" || p.Landing != "squash" {
		t.Fatalf("settings after the repair: %+v", p)
	}
	untouched("the start after the repair")
	// The import, two saves and the repair are the project's revisions, and no
	// project file was written after the import retired it.
	if revisions, err := store.Revisions(project); err != nil || len(revisions) != 4 {
		t.Fatalf("revisions: %d %v", len(revisions), err)
	}
	if got, _ := os.ReadFile(retiredPath); string(got) != retired.data {
		t.Fatalf("the retired file changed:\n%s", got)
	}
	files, err := filepath.Glob(filepath.Join(root, "projects", "*", "config.toml"))
	must(t, err)
	if !reflect.DeepEqual(files, []string{retiredPath}) {
		t.Fatalf("project files: %v", files)
	}
	_ = c
}

func mustRoot(t *testing.T, root string) config.Root {
	t.Helper()
	resolved, err := config.ResolveRoot(root, "")
	must(t, err)
	return resolved
}

func textOf(s string) *string { return &s }

func settingNamed(t *testing.T, snapshot wire.ProjectConfigResponse, name string) wire.ProjectConfigSetting {
	t.Helper()
	for _, s := range snapshot.Settings {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no setting %s", name)
	return wire.ProjectConfigSetting{}
}

// interruptedProject is the identity of the project directory that is not one
// of known: the interrupted registration's.
func interruptedProject(t *testing.T, root string, known ...config.ProjectID) config.ProjectID {
	t.Helper()
	var found []config.ProjectID
next:
	for _, name := range projectDirectories(t, root) {
		for _, id := range known {
			if name == string(id) {
				continue next
			}
		}
		found = append(found, config.ProjectID(name))
	}
	if len(found) != 1 {
		t.Fatalf("interrupted registrations: %v", found)
	}
	return found[0]
}
