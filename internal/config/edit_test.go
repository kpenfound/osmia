package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pid2 = "p_1123456789abcdef0123456789abcdef"

func editFixture(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	write(t, path, text)
	return path
}
func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// writeProjectConfig writes the settings a newly registered project starts
// with as a project file, the way a root of an earlier version holds them.
func writeProjectConfig(path string, p Project) error {
	data, err := ProjectConfigBytes(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func TestProjectConfigBytesLoads(t *testing.T) {
	opts := fixture(t, strings.Replace(topConfig, `"`+pid+`"`, "", 1), projectConfig)
	root, err := ResolveRoot("", opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := ResolveClone(filepath.Join(opts.Home, "clone \"quoted\""), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(clone, 0700); err != nil {
		t.Fatal(err)
	}
	path, err := root.ProjectConfig(pid2)
	if err != nil {
		t.Fatal(err)
	}
	p := Project{Name: "Näme #1", Upstream: "up/repo", Fork: "me/repo", Clone: clone, BaseBranch: "develop"}
	if err := writeProjectConfig(path, p); err != nil {
		t.Fatal(err)
	}
	before := read(t, path)
	if err := writeProjectConfig(path, p); err != nil || read(t, path) != before {
		t.Fatalf("rewrite is not idempotent: %v", err)
	}
	top, err := root.Config()
	if err != nil {
		t.Fatal(err)
	}
	listed := read(t, top)
	write(t, top, strings.Replace(listed, "active_projects = [", `active_projects = ["`+pid2+`", `, 1))
	c, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	if c.Project.ID != pid2 || c.Project.Name != p.Name || c.Project.Clone != clone || c.Project.BaseBranch != "develop" || c.Project.Landing != "commit-per-unit" || c.Project.Capacity.PerWorkstream != 2 {
		t.Fatalf("%+v", c.Project)
	}
	for _, bad := range []Project{{Upstream: "up/repo", Fork: "me/repo", Clone: "relative", BaseBranch: "main"}, {Upstream: "up/repo", Fork: "me/repo", Clone: clone, BaseBranch: "bad..branch"}, {Upstream: "up/repo.git", Fork: "me/repo", Clone: clone, BaseBranch: "main"}} {
		if _, err := ProjectConfigBytes(bad); err == nil {
			t.Fatalf("invalid project written: %+v", bad)
		}
	}
}

func TestLoadWithoutActiveProject(t *testing.T) {
	opts := fixture(t, strings.Replace(topConfig, `"`+pid+`"`, "", 1), projectConfig)
	c, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	if c.HasProject() || c.Project.ID != "" || len(c.ActiveProjects) != 0 {
		t.Fatalf("%+v", c)
	}
	opts = fixture(t, strings.Replace(topConfig, "active_projects = [\""+pid+"\"]\n", "", 1), projectConfig)
	if c, err = Load(opts); err != nil || c.HasProject() {
		t.Fatalf("%+v %v", c, err)
	}
	// A listed project is still required to load.
	opts = fixture(t, topConfig, projectConfig)
	if c, err = Load(opts); err != nil || !c.HasProject() {
		t.Fatalf("%+v %v", c, err)
	}
}
