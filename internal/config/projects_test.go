package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const pidOther = "p_fedcba9876543210fedcba9876543210"

// twoProjects writes a second project beside the fixture's and lists both,
// the fixture's first.
func twoProjects(t *testing.T, second string) Options {
	t.Helper()
	opts := fixture(t, strings.Replace(topConfig, `["`+pid+`"]`, `["`+pid+`", "`+pidOther+`"]`, 1), projectConfig)
	root, err := ResolveRoot("", opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root.String(), "projects", pidOther)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "config.toml"), second)
	return opts
}

func TestSeveralActiveProjectsLoad(t *testing.T) {
	second := strings.Replace(projectConfig, "~/clone", "~/other", 1) + "base_branch = \"develop\"\n[capacity]\nper_workstream = 5\n"
	c, err := Load(twoProjects(t, second))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.ProjectIDs(), []ProjectID{pid, pidOther}) || c.HasProject() {
		t.Fatalf("projects %v, project %q", c.ProjectIDs(), c.Project.ID)
	}
	first, other := c.For(pid), c.For(pidOther)
	if first.Project.ID != pid || first.Project.BaseBranch != "main" || first.Project.Capacity.PerWorkstream != 2 {
		t.Fatalf("first project: %+v", first.Project)
	}
	if other.Project.ID != pidOther || other.Project.BaseBranch != "develop" || other.Project.Capacity.PerWorkstream != 5 {
		t.Fatalf("second project: %+v", other.Project)
	}
	if c.For("p_00000000000000000000000000000000").HasProject() || c.HasProject() {
		t.Fatal("a project that is not active was selected")
	}
	without := c.WithoutProjectID(pid)
	if !slices.Equal(without.ProjectIDs(), []ProjectID{pidOther}) || without.Project.ID != pidOther || !slices.Equal(without.ActiveProjects, []string{pidOther}) {
		t.Fatalf("without the first: %v %q %v", without.ProjectIDs(), without.Project.ID, without.ActiveProjects)
	}
}

func TestOneBadProjectFailsTheLoad(t *testing.T) {
	c, err := Load(twoProjects(t, strings.Replace(projectConfig, `fork = "owner/repo"`, `fork = "upstream/repo"`, 1)))
	var field *FieldError
	if c != nil || !errors.As(err, &field) || field.Field != "fork" || !strings.Contains(field.Path, pidOther) {
		t.Fatalf("load: %v %v", c, err)
	}
}
