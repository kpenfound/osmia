package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const ownerProjectFile = `# Settings of the shop project.
version = 1
checks_timeout = "30m" # raised by hand
upstream = "upstream/repo"
fork = "owner/repo"
# Where the clone lives.
clone = "~/clone"

[notes]
call = ["serve"]
port = 8080

[capacity]
# Keep this low.
git_operations = 3

[hearsay_entities]
shop = "shop-scope"
`

func TestEditTOMLChangesOnlyTheEditedKeys(t *testing.T) {
	got, err := editTOML(ownerProjectFile, []keyEdit{
		{path: []string{"landing"}, value: "squash"},
		{path: []string{"checks_timeout"}, value: "45m"},
		{path: []string{"fork"}, remove: true},
		{path: []string{"capacity", "per_workstream"}, value: 5},
		{path: []string{"capacity", "git_operations"}, remove: true},
		{path: []string{"hearsay_entities", "cart"}, value: "cart-scope"},
		{path: []string{"hearsay_entities", "shop"}, remove: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `# Settings of the shop project.
version = 1
checks_timeout = "45m" # raised by hand
upstream = "upstream/repo"
# Where the clone lives.
clone = "~/clone"
landing = "squash"

[notes]
call = ["serve"]
port = 8080

[capacity]
# Keep this low.
per_workstream = 5

[hearsay_entities]
cart = "cart-scope"
`
	if got != want {
		t.Fatalf("edited text:\n%s\nwant:\n%s", got, want)
	}
}

func TestEditTOMLPlacesNewKeysAndTables(t *testing.T) {
	for _, c := range []struct {
		name, text string
		edits      []keyEdit
		want       string
	}{
		{"new table at the end", "version = 1\nname = \"x\"\n", []keyEdit{{path: []string{"capacity", "per_workstream"}, value: 3}},
			"version = 1\nname = \"x\"\n\n[capacity]\nper_workstream = 3\n"},
		{"no trailing newline", "version = 1\nname = \"x\"", []keyEdit{{path: []string{"landing"}, value: "squash"}},
			"version = 1\nname = \"x\"\nlanding = \"squash\"\n"},
		{"file without keys", "# only a comment\n", []keyEdit{{path: []string{"name"}, value: "x"}},
			"name = \"x\"\n# only a comment\n"},
		{"dotted keys", "version = 1\ncapacity.git_operations = 2\n[notes]\nport = 1\n", []keyEdit{{path: []string{"capacity", "per_workstream"}, value: 4}, {path: []string{"capacity", "git_operations"}, remove: true}},
			"version = 1\ncapacity.per_workstream = 4\n[notes]\nport = 1\n"},
		{"quoted entity", "version = 1\n[hearsay_entities]\n", []keyEdit{{path: []string{"hearsay_entities", "web.shop"}, value: "scope"}},
			"version = 1\n[hearsay_entities]\n\"web.shop\" = \"scope\"\n"},
		{"existing quoted key", "version = 1\n\"name\" = 'old'\n", []keyEdit{{path: []string{"name"}, value: "new \"quoted\""}},
			"version = 1\n\"name\" = \"new \\\"quoted\\\"\"\n"},
		{"carriage returns", "version = 1\r\nname = \"x\"\r\n[capacity]\r\ngit_operations = 2\r\n", []keyEdit{{path: []string{"landing"}, value: "squash"}, {path: []string{"capacity", "per_workstream"}, value: 3}},
			"version = 1\r\nname = \"x\"\r\nlanding = \"squash\"\r\n[capacity]\r\ngit_operations = 2\r\nper_workstream = 3\r\n"},
		{"a multi-line value elsewhere", "version = 1\nnotes = \"\"\"\nname = \"fake\"\n[capacity]\n\"\"\"\nname = \"x\"\n", []keyEdit{{path: []string{"name"}, remove: true}},
			"version = 1\nnotes = \"\"\"\nname = \"fake\"\n[capacity]\n\"\"\"\n"},
		{"removing an absent key", "version = 1\n", []keyEdit{{path: []string{"landing"}, remove: true}}, "version = 1\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := editTOML(c.text, c.edits)
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestEditTOMLRefusesLayoutsItCannotChangeSafely(t *testing.T) {
	for name, text := range map[string]string{
		"inline table": "version = 1\ncapacity = { git_operations = 2 }\n",
		"broken file":  "version = 1\n[capacity\n",
	} {
		if _, err := editTOML(text, []keyEdit{{path: []string{"capacity", "per_workstream"}, value: 3}}); err == nil {
			t.Fatalf("%s was edited", name)
		}
	}
}

func projectEditFixture(t *testing.T, project string) (*Config, Options) {
	t.Helper()
	opts := fixture(t, topConfig+"[profiles.other]\nagent = \"claude\"\nmodel = \"other\"\n", project)
	c, err := load(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c, opts
}

func TestEditProjectValidatesTheResultLikeLoading(t *testing.T) {
	c, opts := projectEditFixture(t, projectConfig)
	pinned := []byte(projectConfig)
	str := func(name, value string) ProjectEdit { return ProjectEdit{Strings: map[string]string{name: value}} }
	for _, tc := range []struct {
		name  string
		edit  ProjectEdit
		field string
	}{
		{"checks timeout out of range", str("checks_timeout", "25h"), "checks_timeout"},
		{"checks timeout too small", str("checks_timeout", "30s"), "checks_timeout"},
		{"malformed upstream", str("upstream", "https://github.com/a/b.git"), "upstream"},
		{"malformed fork", str("fork", "nope"), "fork"},
		{"bad branch", str("base_branch", "a..b"), "base_branch"},
		{"bad landing", str("landing", "sideways"), "landing"},
		{"bad poll", str("pull_request_poll", "1m"), "pull_request_poll"},
		{"bad rebase", str("upstream_rebase", "30s"), "upstream_rebase"},
		{"bad scope", str("hearsay_scope", "Not A Scope"), "hearsay_scope"},
		{"old scope name", str("hearsay_scope", "osmia-ab12"), "hearsay_scope"},
		{"old scope name as entity", ProjectEdit{Entities: map[string]string{"shop": "osmia-ab12"}}, "hearsay_entities"},
		{"not a flag", ProjectEdit{Bools: map[string]bool{"landing": true}}, "landing"},
		{"bad capacity", ProjectEdit{Ints: map[string]int{"capacity.per_workstream": 0}}, "capacity.per_workstream"},
		{"bad git operations", ProjectEdit{Ints: map[string]int{"capacity.git_operations": -1}}, "capacity.git_operations"},
		{"bad entity", ProjectEdit{Entities: map[string]string{"shop": "BAD"}}, "hearsay_entities"},
		{"unknown setting", str("colour", "x"), "colour"},
		{"required upstream cleared", ProjectEdit{Clear: []string{"upstream"}}, "upstream"},
		{"required clone cleared", ProjectEdit{Clear: []string{"clone"}}, "clone"},
		{"set and cleared", ProjectEdit{Strings: map[string]string{"landing": "squash"}, Clear: []string{"landing"}}, "landing"},
	} {
		_, _, err := c.EditProject(pid, opts.Home, pinned, tc.edit)
		var field *FieldError
		if !errors.As(err, &field) || field.Field != tc.field || !strings.Contains(field.Path, pid) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}

	edited, file, err := c.EditProject(pid, opts.Home, pinned, ProjectEdit{
		Strings: map[string]string{"checks_timeout": "30m", "landing": "squash"},
		Ints:    map[string]int{"capacity.per_workstream": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := projectConfig + "checks_timeout = \"30m\"\nlanding = \"squash\"\n\n[capacity]\nper_workstream = 3\n"
	if string(edited) != want {
		t.Fatalf("edited:\n%s\nwant:\n%s", edited, want)
	}
	if p := file.Project; p.ChecksTimeout != "30m" || p.Landing != "squash" || p.Capacity.PerWorkstream != 3 || !file.Set("landing") || file.Set("base_branch") || file.Set("capacity.git_operations") {
		t.Fatalf("decoded: %+v", file)
	}
	write(t, projectFile(opts), string(edited))
	fresh, err := load(opts)
	if err != nil || !reflect.DeepEqual(fresh.Project, file.Project) {
		t.Fatalf("fresh load: %+v %v", fresh.Project, err)
	}
}

func TestEditProjectClearsKeysToTheirDefaults(t *testing.T) {
	text := projectConfig + "checks_timeout = \"30m\"\nhearsay_scope = \"work:main:shop\"\nname = \"Shop\"\n[capacity]\nper_workstream = 7\n[hearsay_entities]\na = \"code:a\"\nb = \"code:b\"\n"
	c, opts := projectEditFixture(t, text)
	if c.Project.Capacity.PerWorkstream != 7 {
		t.Fatalf("fixture: %+v", c.Project)
	}
	edited, file, err := c.EditProject(pid, opts.Home, []byte(text), ProjectEdit{
		Clear:         []string{"checks_timeout", "capacity.per_workstream"},
		Strings:       map[string]string{"hearsay_scope": "", "name": ""},
		ClearEntities: []string{"a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := projectConfig + "[capacity]\n[hearsay_entities]\nb = \"code:b\"\n"
	if string(edited) != want {
		t.Fatalf("edited:\n%s\nwant:\n%s", edited, want)
	}
	p := file.Project
	if p.ChecksTimeout != DefaultChecksTimeout || p.Capacity.PerWorkstream != c.Capacity.PerWorkstream || p.HearsayScope != "" || p.Name != "" || !reflect.DeepEqual(p.HearsayEntities, map[string]string{"b": "code:b"}) {
		t.Fatalf("defaults: %+v", p)
	}
	if file.Set("checks_timeout") || file.Set("capacity.per_workstream") || !file.Set("hearsay_entities") {
		t.Fatal("markers do not follow the file")
	}
}

func TestEditProjectSetsAndClearsFlakeFixes(t *testing.T) {
	c, opts := projectEditFixture(t, projectConfig)
	if c.Project.FlakeFixes {
		t.Fatal("flake_fixes is not off by default")
	}
	edited, file, err := c.EditProject(pid, opts.Home, []byte(projectConfig), ProjectEdit{Bools: map[string]bool{"flake_fixes": true}})
	if err != nil {
		t.Fatal(err)
	}
	if want := projectConfig + "flake_fixes = true\n"; string(edited) != want {
		t.Fatalf("edited:\n%s\nwant:\n%s", edited, want)
	}
	if !file.Project.FlakeFixes || !file.Set("flake_fixes") {
		t.Fatalf("decoded: %+v", file)
	}
	back, file, err := c.EditProject(pid, opts.Home, edited, ProjectEdit{Clear: []string{"flake_fixes"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != projectConfig || file.Project.FlakeFixes || file.Set("flake_fixes") {
		t.Fatalf("cleared: %q %+v", back, file)
	}
}

func TestEditProjectSetsAndClearsTheRepoOptIn(t *testing.T) {
	c, opts := projectEditFixture(t, projectConfig)
	if c.Project.HearsayDeclareRepo {
		t.Fatal("the opt-in is not off by default")
	}
	edited, file, err := c.EditProject(pid, opts.Home, []byte(projectConfig), ProjectEdit{Bools: map[string]bool{"hearsay_declare_repo": true}})
	if err != nil {
		t.Fatal(err)
	}
	if want := projectConfig + "hearsay_declare_repo = true\n"; string(edited) != want {
		t.Fatalf("edited:\n%s\nwant:\n%s", edited, want)
	}
	if !file.Project.HearsayDeclareRepo || !file.Set("hearsay_declare_repo") {
		t.Fatalf("decoded: %+v", file)
	}
	back, file, err := c.EditProject(pid, opts.Home, edited, ProjectEdit{Clear: []string{"hearsay_declare_repo"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != projectConfig || file.Project.HearsayDeclareRepo || file.Set("hearsay_declare_repo") {
		t.Fatalf("cleared: %q %+v", back, file)
	}
}
