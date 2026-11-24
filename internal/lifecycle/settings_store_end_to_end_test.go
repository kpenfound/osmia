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

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/cli"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

type fileState struct {
	data    string
	modTime time.Time
}

func stateOf(t *testing.T, path string) fileState {
	t.Helper()
	data, err := os.ReadFile(path)
	intakefixture.Must(t, err)
	info, err := os.Stat(path)
	intakefixture.Must(t, err)
	return fileState{string(data), info.ModTime()}
}

func osmia(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var out, diag bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"--root", root}, args...), strings.NewReader(""), &out, &diag)
	return code, out.String(), diag.String()
}

func TestSettingsStoreRunsTheWholePath(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	ctx := context.Background()
	opts := fixture(t)
	root := opts.Config.Root
	home := filepath.Dir(root)
	top := filepath.Join(root, "config.toml")

	cloneDir := filepath.Join(home, "clones")
	remote, _ := servicetest.SeedRemote(t, home, "newrepo", "main")
	github := servicetest.NewFakeGitHub(t, "owner")
	github.Remote = func(string) string { return remote }
	opts.Repositories = github.Client()
	legacy, err := os.ReadFile(top)
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.WriteFile(top, append(legacy, fmt.Sprintf("\n[projects]\nrepo_owner = \"owner\"\nclone_dir = %q\n", cloneDir)...), 0400))
	intakefixture.Must(t, os.Chmod(top, 0400))
	baseline := stateOf(t, top)
	untouched := func(step string) {
		t.Helper()
		if got := stateOf(t, top); got != baseline {
			t.Fatalf("%s: the top-level file changed", step)
		}
	}

	s, c := start(t, opts)
	untouched("startup")
	store := projectstore.Open(mustRoot(t, root))
	if got := activeProjects(t, root); !reflect.DeepEqual(got, []string{string(fixturekit.Project)}) {
		t.Fatalf("registry at startup: %v", got)
	}

	first, err := c.ProjectConfig(ctx, fixturekit.Project)
	intakefixture.Must(t, err)
	if first.Pin == "" || first.Revision != 1 || first.Error != nil {
		t.Fatalf("first read: %+v", first)
	}
	saved, err := c.SaveProjectConfig(ctx, fixturekit.Project, api.ProjectConfigEdit{Pin: first.Pin, Set: api.ProjectConfigValues{ChecksTimeout: textOf("45m")}})
	intakefixture.Must(t, err)
	if saved.Outcome != api.ProjectConfigApplied || saved.Pin == first.Pin || saved.Revision != 2 || s.current().For(fixturekit.Project).Project.ChecksTimeout != "45m" {
		t.Fatalf("applied save: %+v", saved)
	}
	_, err = c.SaveProjectConfig(ctx, fixturekit.Project, api.ProjectConfigEdit{Pin: first.Pin, Set: api.ProjectConfigValues{Landing: textOf("squash")}})
	fixturekit.AssertCode(t, err, api.Conflict)
	if got := storedSettings(t, root, fixturekit.Project); !strings.Contains(got, `checks_timeout = "45m"`) || strings.Contains(got, "squash") {
		t.Fatalf("a stale save changed the stored settings:\n%s", got)
	}
	_, err = c.SaveProjectConfig(ctx, fixturekit.Project, api.ProjectConfigEdit{Pin: saved.Pin, Set: api.ProjectConfigValues{ChecksTimeout: textOf("soon")}})
	fixturekit.AssertCode(t, err, api.Validation)
	if !strings.Contains(err.Error(), config.ProjectSettingsLabel(fixturekit.Project)+": checks_timeout: ") || strings.Contains(err.Error(), "config.toml") {
		t.Fatalf("invalid edit: %v", err)
	}
	s.boundary = func(name string) error {
		if name == "project-config-written" {
			return errors.New("the scheduler is unreachable")
		}
		return nil
	}
	pending, err := c.SaveProjectConfig(ctx, fixturekit.Project, api.ProjectConfigEdit{Pin: saved.Pin, Set: api.ProjectConfigValues{Landing: textOf("squash")}})
	intakefixture.Must(t, err)
	s.boundary = nil
	if pending.Outcome != api.ProjectConfigSavedNotApplied || !strings.Contains(pending.Reason, "the scheduler is unreachable") || s.current().For(fixturekit.Project).Project.Landing != "commit-per-unit" {
		t.Fatalf("not applied save: %+v", pending)
	}
	untouched("settings saves")

	edited, err := os.ReadFile(top)
	intakefixture.Must(t, err)
	intakefixture.Must(t, os.Chmod(top, 0600))
	intakefixture.Must(t, os.WriteFile(top, append(edited, "\n[capacity]\nmasons = 7\n"...), 0600))
	intakefixture.Must(t, os.Chmod(top, 0400))
	baseline = stateOf(t, top)
	reloaded, err := c.Reload(ctx)
	intakefixture.Must(t, err)
	now, err := c.Configuration(ctx)
	intakefixture.Must(t, err)
	if reloaded.Digest != now.Digest || now.Effective.Capacity.Masons != 7 || len(now.Effective.Projects) != 1 || now.Effective.Projects[0].Landing != "commit-per-unit" || now.Effective.Projects[0].ChecksTimeout != "45m" {
		t.Fatalf("after the reload: %+v", now.Effective.Projects)
	}
	read, err := c.ProjectConfig(ctx, fixturekit.Project)
	intakefixture.Must(t, err)
	if read.Pin != pending.Pin || !settingNamed(t, read, "landing").Pending || settingNamed(t, read, "landing").Value != "squash" {
		t.Fatalf("the reload did not leave the saved revision pending: %+v", read)
	}
	if now.Drift.Differs || len(now.Drift.Files) != 1 || now.Drift.Files[0].Path != top {
		t.Fatalf("drift: %+v", now.Drift)
	}
	untouched("reload")

	cloneA := filepath.Join(home, "clone-a")
	fixturekit.DemoGit(t, home, "init", "--quiet", cloneA)
	addedA, err := c.AddProject(ctx, fixturekit.Request(cloneA))
	intakefixture.Must(t, err)
	req := api.ProjectCreateRequest{Name: "New Project", Repo: "newrepo", Description: "A new project", Charter: creationCharter}
	createdB, err := c.CreateProject(ctx, req)
	intakefixture.Must(t, err)
	if got := activeProjects(t, root); !reflect.DeepEqual(got, []string{string(fixturekit.Project), string(addedA.Project.ID), string(createdB.Project.ID)}) {
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
	intakefixture.Must(t, err)
	if got := activeProjects(t, root); !reflect.DeepEqual(got, []string{string(fixturekit.Project), string(createdB.Project.ID)}) {
		t.Fatalf("registry after the removal: %v", got)
	}
	untouched("add, create and remove")

	cloneC := filepath.Join(home, "clone-c")
	fixturekit.DemoGit(t, home, "init", "--quiet", cloneC)
	s.boundary = func(name string) error {
		if name == "trace-created" {
			return errors.New("crash")
		}
		return nil
	}
	_, err = c.AddProject(ctx, fixturekit.Request(cloneC))
	fixturekit.AssertCode(t, err, api.Internal)
	s.boundary = nil
	interrupted := interruptedProject(t, root, fixturekit.Project, addedA.Project.ID, createdB.Project.ID)

	intakefixture.Must(t, s.Close())
	s, c = start(t, opts)
	want := []string{string(fixturekit.Project), string(createdB.Project.ID), string(interrupted)}
	if got := activeProjects(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("registry after the restart: %v, want %v", got, want)
	}
	now, err = c.Configuration(ctx)
	intakefixture.Must(t, err)
	if !reflect.DeepEqual(now.Effective.ActiveProjects, want) {
		t.Fatalf("effective.active_projects %v, want %v", now.Effective.ActiveProjects, want)
	}
	if p := s.current().For(fixturekit.Project).Project; p.ChecksTimeout != "45m" || p.Landing != "squash" {
		t.Fatalf("the last saved settings after the restart: %+v", p)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "projects")); err != nil || len(entries) != 3 {
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
	cloneOfProject := s.current().For(fixturekit.Project).Project.Clone

	intakefixture.Must(t, s.Close())
	intakefixture.Must(t, os.RemoveAll(cloneOfProject))
	intakefixture.Must(t, os.WriteFile(cloneOfProject, []byte("not a directory"), 0600))
	_, err = Start(ctx, opts)
	if err == nil {
		t.Fatal("startup did not refuse stored settings that do not load")
	}
	for _, want := range []string{config.ProjectSettingsLabel(fixturekit.Project), "clone", "must be a directory", "osmia project settings " + string(fixturekit.Project) + " set clone=VALUE"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal lacks %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "config.toml") || strings.Contains(err.Error(), "by hand") {
		t.Fatalf("the refusal points at a file: %v", err)
	}
	repaired := filepath.Join(home, "repaired-clone")
	fixturekit.DemoGit(t, home, "init", "--quiet", repaired)
	code, text, diag = osmia(t, root, "project", "settings", string(fixturekit.Project), "set", "clone="+repaired)
	if code != 0 {
		t.Fatalf("repair: %d %s %s", code, text, diag)
	}
	untouched("the refused start and the repair")
	s, c = start(t, opts)
	if got := activeProjects(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("registry after the repair: %v, want %v", got, want)
	}
	if p := s.current().For(fixturekit.Project).Project; p.Clone != repaired || p.ChecksTimeout != "45m" || p.Landing != "squash" {
		t.Fatalf("settings after the repair: %+v", p)
	}
	untouched("the start after the repair")
	if revisions, err := store.Revisions(fixturekit.Project); err != nil || len(revisions) != 4 {
		t.Fatalf("revisions: %d %v", len(revisions), err)
	}
	files, err := filepath.Glob(filepath.Join(root, "projects", "*", "config.toml"))
	intakefixture.Must(t, err)
	if len(files) != 0 {
		t.Fatalf("project files: %v", files)
	}
	_ = c
}

func mustRoot(t *testing.T, root string) config.Root {
	t.Helper()
	resolved, err := config.ResolveRoot(root, "")
	intakefixture.Must(t, err)
	return resolved
}

func textOf(s string) *string { return &s }

func settingNamed(t *testing.T, snapshot api.ProjectConfigResponse, name string) api.ProjectConfigSetting {
	t.Helper()
	for _, s := range snapshot.Settings {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no setting %s", name)
	return api.ProjectConfigSetting{}
}

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
