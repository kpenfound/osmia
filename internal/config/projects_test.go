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
	c, err := Load(twoProjects(t, strings.Replace(projectConfig, `fork = "owner/repo"`, `fork = "invalid"`, 1)))
	var field *FieldError
	if c != nil || !errors.As(err, &field) || field.Field != "fork" || !strings.Contains(field.Path, pidOther) {
		t.Fatalf("load: %v %v", c, err)
	}
}

func TestOptionalForkRoundTrips(t *testing.T) {
	for _, fork := range []string{"", "UPSTREAM/repo", "owner/repo"} {
		t.Run(fork, func(t *testing.T) {
			opts := fixture(t, topConfig, strings.Replace(projectConfig, `fork = "owner/repo"`, `fork = "`+fork+`"`, 1))
			c, err := Load(opts)
			if err != nil {
				t.Fatal(err)
			}
			want := fork
			if strings.EqualFold(fork, c.Project.Upstream) {
				want = ""
			}
			if c.Project.Fork != want {
				t.Fatalf("fork %q, want %q", c.Project.Fork, want)
			}
			target := want
			if target == "" {
				target = c.Project.Upstream
			}
			if c.Project.PushRepository() != target {
				t.Fatal(c.Project)
			}
			path := filepath.Join(c.Root.String(), "projects", pid, "config.toml")
			if err := WriteProjectConfig(path, c.Project); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if want == "" && strings.Contains(string(data), "fork =") {
				t.Fatalf("fork persisted: %s", data)
			}
			reloaded, err := Load(opts)
			if err != nil || reloaded.Project.Fork != want {
				t.Fatalf("reload %v: %v", reloaded, err)
			}
		})
	}
}

// WithLoadedProjects lists already loaded projects in the order given and
// reads no project file, so it works once every project file is gone.
func TestWithLoadedProjectsReadsNoFile(t *testing.T) {
	second := strings.Replace(projectConfig, "~/clone", "~/other", 1)
	c, err := Load(twoProjects(t, second))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []ProjectID{pid, pidOther} {
		path, err := c.Root.ProjectConfig(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	reversed := c.WithLoadedProjects([]Project{c.For(pidOther).Project, c.For(pid).Project})
	if !slices.Equal(reversed.ProjectIDs(), []ProjectID{pidOther, pid}) || !slices.Equal(reversed.ActiveProjects, []string{pidOther, pid}) || reversed.HasProject() {
		t.Fatalf("projects %v, active %v", reversed.ProjectIDs(), reversed.ActiveProjects)
	}
	one := c.WithLoadedProjects([]Project{c.For(pid).Project})
	if one.Project.ID != pid || !slices.Equal(one.ActiveProjects, []string{pid}) {
		t.Fatalf("one project: %q %v", one.Project.ID, one.ActiveProjects)
	}
	if !slices.Equal(c.ProjectIDs(), []ProjectID{pid, pidOther}) {
		t.Fatal("the receiver changed")
	}
}
