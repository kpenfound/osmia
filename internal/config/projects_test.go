package config

import (
	"slices"
	"strings"
	"testing"
)

const pidOther = "p_fedcba9876543210fedcba9876543210"

func twoProjects(t *testing.T, second string) *Config {
	t.Helper()
	opts := fixture(t, topConfig, projectConfig)
	c, err := load(opts)
	if err != nil {
		t.Fatal(err)
	}
	file, err := c.DecodeProject(pidOther, opts.Home, []byte(second))
	if err != nil {
		t.Fatal(err)
	}
	return c.WithLoadedProjects([]Project{c.Projects[0], file.Project})
}

func TestForSelectsOneOfSeveralActiveProjects(t *testing.T) {
	second := strings.Replace(projectConfig, "~/clone", "~/other", 1) + "base_branch = \"develop\"\n[capacity]\nper_workstream = 5\n"
	c := twoProjects(t, second)
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
}

func TestWithoutProjectIDKeepsTheOthersInOrder(t *testing.T) {
	c := twoProjects(t, strings.Replace(projectConfig, "~/clone", "~/other", 1))
	without := c.WithoutProjectID(pid)
	if !slices.Equal(without.ProjectIDs(), []ProjectID{pidOther}) || without.Project.ID != pidOther || !slices.Equal(without.ActiveProjects, []string{pidOther}) {
		t.Fatalf("without the first: %v %q %v", without.ProjectIDs(), without.Project.ID, without.ActiveProjects)
	}
}

func TestOptionalForkRoundTrips(t *testing.T) {
	for _, fork := range []string{"", "UPSTREAM/repo", "owner/repo"} {
		t.Run(fork, func(t *testing.T) {
			opts := fixture(t, topConfig, strings.Replace(projectConfig, `fork = "owner/repo"`, `fork = "`+fork+`"`, 1))
			c, err := load(opts)
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
			data, err := ProjectConfigBytes(c.Project)
			if err != nil {
				t.Fatal(err)
			}
			if want == "" && strings.Contains(string(data), "fork =") {
				t.Fatalf("fork persisted: %s", data)
			}
			write(t, projectFile(opts), string(data))
			reloaded, err := load(opts)
			if err != nil || reloaded.Project.Fork != want {
				t.Fatalf("reload %v: %v", reloaded, err)
			}
		})
	}
}

func TestWithLoadedProjectsKeepsTheOrderGiven(t *testing.T) {
	c := twoProjects(t, strings.Replace(projectConfig, "~/clone", "~/other", 1))
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
