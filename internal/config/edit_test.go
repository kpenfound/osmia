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

func TestActiveProjectEditsPreserveText(t *testing.T) {
	const commented = `# Owner's notes stay here.
version = 1
active_projects = [] # none yet

[profiles.default] # keep this profile
agent = "claude"
model = "test-model"
`
	path := editFixture(t, commented)
	if err := AddActiveProject(path, pid); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	want := strings.Replace(commented, "active_projects = []", `active_projects = ["`+pid+`"]`, 1)
	if got != want {
		t.Fatalf("add changed unrelated text:\n%s", got)
	}
	if err := AddActiveProject(path, pid); err != nil || read(t, path) != want {
		t.Fatalf("repeated add changed the file: %v\n%s", err, read(t, path))
	}
	if err := RemoveActiveProject(path, pid); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != commented {
		t.Fatalf("remove changed unrelated text:\n%s", got)
	}
	if err := RemoveActiveProject(path, pid); err != nil || read(t, path) != commented {
		t.Fatalf("repeated remove changed the file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode changed: %v %v", info, err)
	}
}

func TestActiveProjectEditShapes(t *testing.T) {
	cases := []struct{ name, before, add, remove string }{
		{"missing key", "version = 1\n[profiles.default]\nagent = 'claude'\nmodel = 'm'\n", "version = 1\nactive_projects = [\"" + pid + "\"]\n[profiles.default]\nagent = 'claude'\nmodel = 'm'\n", "version = 1\nactive_projects = []\n[profiles.default]\nagent = 'claude'\nmodel = 'm'\n"},
		{"missing key and version", "[profiles.default]\nagent = 'claude'\nmodel = 'm'\n", "active_projects = [\"" + pid + "\"]\n[profiles.default]\nagent = 'claude'\nmodel = 'm'\n", "active_projects = []\n[profiles.default]\nagent = 'claude'\nmodel = 'm'\n"},
		{"multi-line", "active_projects = [\n  \"" + pid2 + "\", # first\n]\n", "active_projects = [\n  \"" + pid2 + "\", \"" + pid + "\", # first\n]\n", "active_projects = [\n  \"" + pid2 + "\", # first\n]\n"},
		{"literal strings", "active_projects = ['" + pid2 + "']\n", "active_projects = ['" + pid2 + "', \"" + pid + "\"]\n", "active_projects = ['" + pid2 + "']\n"},
		{"tabs", "active_projects\t=\t[\t]\n", "active_projects\t=\t[\"" + pid + "\"\t]\n", "active_projects\t=\t[\t]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := editFixture(t, tc.before)
			if err := AddActiveProject(path, pid); err != nil {
				t.Fatal(err)
			}
			if got := read(t, path); got != tc.add {
				t.Fatalf("add:\n%s\nwant:\n%s", got, tc.add)
			}
			if err := RemoveActiveProject(path, pid); err != nil {
				t.Fatal(err)
			}
			if got := read(t, path); got != tc.remove {
				t.Fatalf("remove:\n%s\nwant:\n%s", got, tc.remove)
			}
		})
	}
	// Removing the first of two keeps the second; removing the only one leaves [].
	path := editFixture(t, "active_projects = [\""+pid+"\", \""+pid2+"\"]\n")
	if err := RemoveActiveProject(path, pid); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "active_projects = [\""+pid2+"\"]\n" {
		t.Fatalf("remove first: %s", got)
	}
	if err := RemoveActiveProject(path, pid2); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "active_projects = []\n" {
		t.Fatalf("remove last: %s", got)
	}
}

func TestActiveProjectEditRefusesUnsafeText(t *testing.T) {
	for name, text := range map[string]string{
		"table key":    "[section]\nactive_projects = []\n",
		"unterminated": "active_projects = [\"" + pid2 + "\"\n",
		"not array":    "active_projects = \"" + pid2 + "\"\n",
		"malformed":    "active_projects = [\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := editFixture(t, text)
			err := AddActiveProject(path, pid)
			if name == "table key" {
				// The key belongs to a table; the top-level key is inserted.
				if err != nil || !strings.HasPrefix(read(t, path), "active_projects = [\""+pid+"\"]\n[section]") {
					t.Fatalf("%v\n%s", err, read(t, path))
				}
				return
			}
			if err == nil || read(t, path) != text {
				t.Fatalf("unsafe edit accepted: %v\n%s", err, read(t, path))
			}
		})
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Symlink("elsewhere", path); err != nil {
		t.Fatal(err)
	}
	if err := AddActiveProject(path, pid); err == nil {
		t.Fatal("symlink edited")
	}
	if err := AddActiveProject(editFixture(t, "version = 1\n"), "bad"); err == nil {
		t.Fatal("invalid identity accepted")
	}
}

func TestWriteProjectConfigLoads(t *testing.T) {
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
	if err := WriteProjectConfig(path, p); err != nil {
		t.Fatal(err)
	}
	before := read(t, path)
	if err := WriteProjectConfig(path, p); err != nil || read(t, path) != before {
		t.Fatalf("rewrite is not idempotent: %v", err)
	}
	if err := AddActiveProject(filepath.Join(root.String(), "config.toml"), pid2); err != nil {
		t.Fatal(err)
	}
	c, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	if c.Project.ID != pid2 || c.Project.Name != p.Name || c.Project.Clone != clone || c.Project.BaseBranch != "develop" || c.Project.Landing != "commit-per-unit" || c.Project.Capacity.PerWorkstream != 2 {
		t.Fatalf("%+v", c.Project)
	}
	for _, bad := range []Project{{Upstream: "up/repo", Fork: "up/repo", Clone: clone, BaseBranch: "main"}, {Upstream: "up/repo", Fork: "me/repo", Clone: "relative", BaseBranch: "main"}, {Upstream: "up/repo", Fork: "me/repo", Clone: clone, BaseBranch: "bad..branch"}, {Upstream: "up/repo.git", Fork: "me/repo", Clone: clone, BaseBranch: "main"}} {
		if err := WriteProjectConfig(filepath.Join(t.TempDir(), "config.toml"), bad); err == nil {
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
