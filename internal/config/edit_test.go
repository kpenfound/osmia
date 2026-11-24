package config

import (
	"os"
	"path/filepath"
	"testing"
)

const pid2 = "p_1123456789abcdef0123456789abcdef"

func TestProjectConfigBytesLoads(t *testing.T) {
	opts := fixture(t, topConfig, projectConfig)
	clone, err := ResolveClone(filepath.Join(opts.Home, "clone \"quoted\""), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(clone, 0700); err != nil {
		t.Fatal(err)
	}
	p := Project{Name: "Näme #1", Upstream: "up/repo", Fork: "me/repo", Clone: clone, BaseBranch: "develop"}
	data, err := ProjectConfigBytes(p)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ProjectConfigBytes(p)
	if err != nil || string(again) != string(data) {
		t.Fatalf("rewrite is not idempotent: %v", err)
	}
	top, err := LoadTopLevel(opts)
	if err != nil {
		t.Fatal(err)
	}
	file, err := top.DecodeProject(pid2, opts.Home, data)
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Project; got.ID != pid2 || got.Name != p.Name || got.Clone != clone || got.BaseBranch != "develop" || got.Landing != "commit-per-unit" || got.Capacity.PerWorkstream != 2 {
		t.Fatalf("%+v", got)
	}
	for _, bad := range []Project{{Upstream: "up/repo", Fork: "me/repo", Clone: "relative", BaseBranch: "main"}, {Upstream: "up/repo", Fork: "me/repo", Clone: clone, BaseBranch: "bad..branch"}, {Upstream: "up/repo.git", Fork: "me/repo", Clone: clone, BaseBranch: "main"}} {
		if _, err := ProjectConfigBytes(bad); err == nil {
			t.Fatalf("invalid project written: %+v", bad)
		}
	}
}

func TestLoadTopLevelReadsNoProjectSettings(t *testing.T) {
	c, err := LoadTopLevel(fixture(t, topConfig, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.HasProject() || len(c.Projects) != 0 || len(c.ActiveProjects) != 0 {
		t.Fatalf("%+v", c)
	}
}
