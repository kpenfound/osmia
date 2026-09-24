package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pid = "p_0123456789abcdef0123456789abcdef"
const wid = "w_0123456789abcdef0123456789abcdef"
const topConfig = `version = 1
active_projects = ["` + pid + `"]
[profiles.default]
agent = "claude"
model = "test-model"
`
const projectConfig = `version = 1
upstream = "upstream/repo"
fork = "owner/repo"
clone = "~/clone"
`

func fixture(t *testing.T, top, project string) Options {
	t.Helper()
	// Short roots keep the Unix socket portable even for long subtest names.
	home, err := os.MkdirTemp("", "oc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	root := filepath.Join(home, ".osmia")
	dir := filepath.Join(root, "projects", pid)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "config.toml"), top)
	write(t, filepath.Join(dir, "config.toml"), project)
	return Options{Home: home}
}
func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestDefaults(t *testing.T) {
	opts := fixture(t, topConfig, projectConfig)
	c, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := ResolveRoot("", opts.Home)
	if c.Root != root || c.Listen.Socket != filepath.Join(root.String(), "osmia.sock") {
		t.Fatalf("root/socket: %+v", c)
	}
	if c.Capacity != (Capacity{4, 2, 3, 2}) || c.Shed != (Shed{3, 3}) || c.Mason.MaxCleanTurns != 3 {
		t.Fatalf("numeric defaults: %+v", c)
	}
	if c.Project.ID != pid || c.Project.BaseBranch != "main" || c.Project.Landing != "commit-per-unit" || c.Project.Capacity.PerWorkstream != 2 {
		t.Fatalf("project: %+v", c.Project)
	}
	if c.EventWindow() != 5*time.Second {
		t.Fatalf("event window default: %v", c.EventWindow())
	}
	if c.Profiles["default"].Timeout != "45m" || c.Profiles["default"].Effort != "medium" || len(c.Roles) != 7 {
		t.Fatalf("profile defaults: %+v", c)
	}
	p, settings, err := c.Execution("mason", "default")
	if err != nil || p.Timeout != 45*time.Minute || p.Backend != "claude" || settings.Mode != "none" {
		t.Fatalf("adapter inputs: %+v %+v %v", p, settings, err)
	}
	if _, err := os.Stat(c.Listen.Socket); !os.IsNotExist(err) {
		t.Fatalf("loader created socket: %v", err)
	}
	if _, err := os.Stat(c.Project.Clone); !os.IsNotExist(err) {
		t.Fatalf("loader created clone: %v", err)
	}
}

func TestBudgetLimits(t *testing.T) {
	c, err := Load(fixture(t, topConfig+"[budget]\nper_session = '5.00'\nper_unit = '40.00'\nper_day = '150.00'\n", projectConfig))
	if err != nil || c.Budget.SessionLimitUSD() != 5 || c.Budget.PerUnit != "40.00" || c.Budget.PerDay != "150.00" {
		t.Fatalf("budget: %+v %v", c, err)
	}
	p, _, err := c.Execution("mason", "default")
	if err != nil || p.CostLimitUSD != 5 {
		t.Fatalf("execution cap: %+v %v", p, err)
	}
	for _, field := range []string{"per_session", "per_unit", "per_day"} {
		for _, value := range []string{"'0'", "'-1'", "'abc'", "'1e3'", "'NaN'", "''"} {
			_, err := Load(fixture(t, topConfig+"[budget]\n"+field+" = "+value+"\n", projectConfig))
			if err == nil || !strings.Contains(err.Error(), "budget."+field) {
				t.Fatalf("%s %s: %v", field, value, err)
			}
		}
	}
	without, err := Load(fixture(t, topConfig, projectConfig))
	if err != nil || without.Budget.SessionLimitUSD() != 0 {
		t.Fatalf("absent budget: %+v %v", without, err)
	}
}

func TestProjectClassifier(t *testing.T) {
	for _, tc := range []struct{ setting, want string }{{"", ""}, {"classifier = 'default'\n", "default"}} {
		c, err := Load(fixture(t, topConfig, projectConfig+tc.setting))
		if err != nil || c.Project.Classifier != tc.want {
			t.Fatalf("classifier %q: %+v %v", tc.setting, c, err)
		}
	}
	if _, err := Load(fixture(t, topConfig, projectConfig+"classifier = 'missing'\n")); err == nil || !strings.Contains(err.Error(), "classifier: unknown profile missing") {
		t.Fatalf("unknown classifier: %v", err)
	}
	top := topConfig + "[profiles.other]\nagent = 'codex'\nmodel = 'other'\n[roles.mason]\nsandbox = 'claude'\n"
	if _, err := Load(fixture(t, top, projectConfig+"classifier = 'other'\n")); err == nil || !strings.Contains(err.Error(), "claude classifier") {
		t.Fatalf("incompatible classifier sandbox: %v", err)
	}
}
func TestExplicit(t *testing.T) {
	top := topConfig + `effort = "high"
timeout = "2m"
max_turns = 8
fallback = "backup"
[profiles.backup]
agent = "codex"
model = "other-model"
[listen]
socket = "local.sock"
[capacity]
masons = 1
reviewers = 1
committee = 1
per_workstream = 5
[shed]
max_rounds = 1
max_bounces = 2
[mason]
max_clean_turns = 2
[events]
window = "250ms"
[roles.mason]
profile = "default"
sandbox = "container"
image = "test-image:1"
`
	opts := fixture(t, top, projectConfig+`name = "A display name"
base_branch = "release/next"
landing = "squash"
[capacity]
per_workstream = 3
`)
	c, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	if c.Capacity != (Capacity{1, 1, 1, 5}) || c.Shed != (Shed{1, 2}) || c.Project.Capacity.PerWorkstream != 3 || c.Project.Landing != "squash" || c.Project.BaseBranch != "release/next" || c.Project.Name != "A display name" {
		t.Fatalf("overrides: %+v", c)
	}
	if c.EventWindow() != 250*time.Millisecond {
		t.Fatalf("event window: %v", c.EventWindow())
	}
	p, settings, err := c.Execution("mason", "backup")
	if err != nil || p.Backend != "codex" || settings.Mode != "container" || settings.Image != "test-image:1" {
		t.Fatalf("fallback: %+v %+v %v", p, settings, err)
	}
	if !strings.HasSuffix(c.Listen.Socket, "/local.sock") {
		t.Fatal(c.Listen.Socket)
	}
	if _, _, err := c.Execution("bogus", "default"); err == nil {
		t.Fatal("unknown role accepted")
	}
	if p, err := c.NamedProfile("backup"); err != nil || p.Name != "backup" || p.Backend != "codex" || p.Model != "other-model" || p.Effort != "medium" || p.Timeout != 45*time.Minute {
		t.Fatalf("named profile: %+v %v", p, err)
	}
	if p, err := c.NamedProfile("default"); err != nil || p.Effort != "high" || p.Timeout != 2*time.Minute || p.MaxTurns != 8 {
		t.Fatalf("named default profile: %+v %v", p, err)
	}
	if _, err := c.NamedProfile("missing"); err == nil {
		t.Fatal("unknown named profile accepted")
	}
	if _, _, err := c.Execution("mason", "missing"); err == nil {
		t.Fatal("unknown profile accepted")
	}
}
func TestInvalid(t *testing.T) {
	cases := []struct{ name, top, project, want string }{
		{"syntax", "version = [", "", "config.toml:"},
		{"version", strings.Replace(topConfig, "version = 1", "version = 2", 1), "", "version:"},
		{"missing version", strings.Replace(topConfig, "version = 1", "", 1), "", "version:"},
		{"unknown", topConfig + "oops = 1\n", "", "profiles.default.oops"},
		{"empty unknown table", topConfig + "[unexpected]\n", "", "unexpected"},
		{"internal field", "Root = 'oops'\n" + topConfig, "", "Root"},
		{"key case", strings.Replace(topConfig, "model =", "Model =", 1), "", "profiles.default.Model"},
		{"socket state collision", topConfig + "[listen]\nsocket = 'runtime.json'\n", "", "direct child"},
		{"duplicate key", topConfig + "model = 'other'\n", "", "config.toml:"},

		{"two active", strings.Replace(topConfig, `"`+pid+`"`, `"`+pid+`", "p_1123456789abcdef0123456789abcdef"`, 1), "", "unsupported"},
		{"duplicate identity", strings.Replace(topConfig, `"`+pid+`"`, `"`+pid+`", "`+pid+`"`, 1), "", "identity already exists"},
		{"traversal", strings.Replace(topConfig, pid, "../escape", 1), "", "active_projects[0]"},
		{"zero capacity", topConfig + "[capacity]\nmasons = 0\n", "", "capacity.masons"},
		{"negative reviews", topConfig + "[capacity]\nreviewers = -1\n", "", "capacity.reviewers"},
		{"zero committee", topConfig + "[capacity]\ncommittee = 0\n", "", "capacity.committee"},
		{"zero wip", topConfig + "[capacity]\nper_workstream = 0\n", "", "capacity.per_workstream"},
		{"negative rounds", topConfig + "[shed]\nmax_rounds = -1\n", "", "shed.max_rounds"},
		{"zero bounces", topConfig + "[shed]\nmax_bounces = 0\n", "", "shed.max_bounces"},
		{"zero clean turns", topConfig + "[mason]\nmax_clean_turns = 0\n", "", "mason.max_clean_turns"},
		{"zero event window", topConfig + "[events]\nwindow = \"0s\"\n", "", "events.window"},
		{"malformed event window", topConfig + "[events]\nwindow = \"soon\"\n", "", "events.window"},
		{"unknown events key", topConfig + "[events]\nlimit = 1\n", "", "events.limit"},
		{"wrong type", topConfig + "[capacity]\nmasons = 'many'\n", "", "config.toml:"},
		{"backend", strings.Replace(topConfig, "claude", "unknown", 1), "", "profiles.default.agent"},
		{"model", strings.Replace(topConfig, "test-model", " ", 1), "", "profiles.default.model"},
		{"effort", topConfig + "effort = 'unknown'\n", "", "profiles.default.effort"},
		{"timeout", topConfig + "timeout = '0s'\n", "", "profiles.default.timeout"},
		{"empty timeout", topConfig + "timeout = ''\n", "", "profiles.default.timeout"},
		{"negative turns", topConfig + "max_turns = -1\n", "", "profiles.default.max_turns"},
		{"unsupported turns", strings.Replace(topConfig, "claude", "codex", 1) + "max_turns = 1\n", "", "only claude"},
		{"missing fallback", topConfig + "fallback = 'missing'\n", "", "profiles.default.fallback"},
		{"self cycle", topConfig + "fallback = 'default'\n", "", "fallback cycle"},
		{"indirect cycle", topConfig + "fallback = 'other'\n[profiles.other]\nagent = 'codex'\nmodel = 'test'\nfallback = 'default'\n", "", "fallback cycle"},
		{"unknown role", topConfig + "[roles.fake]\nprofile = 'default'\n", "", "roles.fake"},
		{"missing binding", topConfig + "[roles.mason]\nprofile = 'missing'\n", "", "roles.mason.profile"},
		{"invalid sandbox", topConfig + "[roles.mason]\nsandbox = 'unsafe'\n", "", "roles.mason.sandbox"},
		{"missing image", topConfig + "[roles.mason]\nsandbox = 'container'\n", "", "roles.mason.image"},
		{"unused image", topConfig + "[roles.mason]\nimage = 'image'\n", "", "roles.mason.image"},
		{"fallback sandbox", topConfig + "fallback = 'other'\n[profiles.other]\nagent = 'codex'\nmodel = 'test'\n[roles.mason]\nsandbox = 'claude'\n", "", "entire fallback chain"},
		{"tailnet", topConfig + "[listen]\ntailnet = ''\n", "", "unsupported in M1"},
		{"hearsay", topConfig + "[hearsay]\n", "", "unsupported in M1"},
		{"notify", topConfig + "[notify]\nwebhook = 'https://example.com'\n", "", "unsupported in M1"},
		{"empty socket", topConfig + "[listen]\nsocket = ''\n", "", "listen.socket"},
		{"socket escape", topConfig + "[listen]\nsocket = '../outside.sock'\n", "", "listen.socket"},
		{"socket directory", topConfig + "[listen]\nsocket = '.'\n", "", "direct child"},
		{"long socket", topConfig + "[listen]\nsocket = '" + strings.Repeat("a", 104) + ".sock'\n", "", "103 bytes"},
		{"project version", "", strings.Replace(projectConfig, "version = 1", "version = 2", 1), "version:"},
		{"project unknown", "", projectConfig + "oops = 1\n", "oops"},
		{"project unsupported", "", projectConfig + "upstream_rebase = '6h'\n", "unsupported in M1"},
		{"repository url", "", strings.Replace(projectConfig, "upstream/repo", "https://host/upstream/repo", 1), "upstream:"},
		{"same fork", "", strings.Replace(projectConfig, "owner/repo", "UPSTREAM/repo", 1), "fork:"},
		{"missing clone", "", strings.Replace(projectConfig, `clone = "~/clone"`, "", 1), "clone:"},
		{"clone in root", "", strings.Replace(projectConfig, "~/clone", "~/.osmia/clone", 1), "non-nested"},
		{"root in clone", "", strings.Replace(projectConfig, "~/clone", "~", 1), "non-nested"},
		{"tilde user", "", strings.Replace(projectConfig, "~/clone", "~other/clone", 1), "home expansion"},
		{"branch", "", projectConfig + "base_branch = 'bad..branch'\n", "base_branch:"},
		{"landing", "", projectConfig + "landing = 'merge'\n", "landing:"},
		{"project wip", "", projectConfig + "[capacity]\nper_workstream = -1\n", "capacity.per_workstream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			top, project := tc.top, tc.project
			if top == "" {
				top = topConfig
			}
			if project == "" {
				project = projectConfig
			}
			c, err := Load(fixture(t, top, project))
			if c != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got config=%+v err=%v; want nil and %q", c, err, tc.want)
			}
		})
	}
}
func TestMissingFiles(t *testing.T) {
	for _, file := range []string{"config.toml", "projects/" + pid + "/config.toml"} {
		t.Run(file, func(t *testing.T) {
			opts := fixture(t, topConfig, projectConfig)
			if err := os.Remove(filepath.Join(opts.Home, ".osmia", file)); err != nil {
				t.Fatal(err)
			}
			if c, err := Load(opts); c != nil || err == nil || !strings.Contains(err.Error(), file) {
				t.Fatalf("%+v %v", c, err)
			}
		})
	}
}
func TestPathsAndIdentity(t *testing.T) {
	home := t.TempDir()
	r, err := ResolveRoot("", home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.String()); !os.IsNotExist(err) {
		t.Fatal("root was created")
	}
	explicit, err := ResolveRoot(filepath.Join(home, "x", "..", "explicit"), home)
	if err != nil || !strings.HasSuffix(explicit.String(), "/explicit") {
		t.Fatalf("%+v %v", explicit, err)
	}
	p, err := NewProjectID()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProjectID(string(p))
	if parsed != p || err != nil {
		t.Fatal(parsed, err)
	}
	w, err := NewWorkstreamID()
	if err != nil {
		t.Fatal(err)
	}
	parsedW, err := ParseWorkstreamID(string(w))
	if parsedW != w || err != nil {
		t.Fatal(parsedW, err)
	}
	if !errors.Is(CheckProjectIDs(p, p), ErrCollision) || !errors.Is(CheckWorkstreamIDs(w, w), ErrCollision) {
		t.Fatal("collision not detected")
	}
	for _, bad := range []string{"", "..", "/absolute", "a/b", "p_ABCDEF0123456789abcdef0123456789", wid} {
		if _, err := r.ProjectTrace(ProjectID(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := r.Workstream(p, WorkstreamID("../escape")); err == nil {
		t.Fatal("workstream traversal")
	}
	for _, get := range []func() (string, error){r.Config, r.Runtime, r.Socket, func() (string, error) { return r.ProjectTrace(p) }, func() (string, error) { return r.ProjectConfig(p) }, func() (string, error) { return r.Workstream(p, w) }} {
		path, err := get()
		if err != nil || !beneath(r.String(), path) {
			t.Fatalf("%s %v", path, err)
		}
	}
}
func TestSymlinkBoundaries(t *testing.T) {
	for _, target := range []string{"outside", "inside"} {
		t.Run(target, func(t *testing.T) {
			opts := fixture(t, topConfig, projectConfig)
			root := filepath.Join(opts.Home, ".osmia")
			projects := filepath.Join(root, "projects")
			destination := filepath.Join(opts.Home, "elsewhere")
			if target == "inside" {
				destination = filepath.Join(root, "alias")
			}
			if err := os.Rename(projects, destination); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(destination, projects); err != nil {
				t.Fatal(err)
			}
			if c, err := Load(opts); c != nil || err == nil {
				t.Fatalf("alias accepted: %+v %v", c, err)
			}
		})
	}
	opts := fixture(t, topConfig, projectConfig)
	if err := os.Symlink(filepath.Join(opts.Home, ".osmia"), filepath.Join(opts.Home, "clone")); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(opts); c != nil || err == nil || !strings.Contains(err.Error(), "non-nested") {
		t.Fatalf("clone alias: %+v %v", c, err)
	}
}
func TestMetadataDoesNotChangeIdentity(t *testing.T) {
	opts := fixture(t, topConfig, projectConfig)
	before, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := before.Root.ProjectConfig(before.Project.ID)
	write(t, path, strings.Replace(projectConfig, "~/clone", "~/renamed-clone", 1)+"name = 'New display name'\n")
	after, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := before.Root.Workstream(before.Project.ID, WorkstreamID(wid))
	b, _ := after.Root.Workstream(after.Project.ID, WorkstreamID(wid))
	if before.Project.ID != after.Project.ID || a != b {
		t.Fatal("mutable metadata changed identity")
	}
}
func TestBranchNames(t *testing.T) {
	for _, s := range []string{"main", "feature/name", "release-1.2"} {
		if !ValidBranch(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"", "@", "-x", "a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\\b", "a..b", "a@{b", "a.", "a\nb", "a//b", ".a", "a.lock", "/a", "a/"} {
		if ValidBranch(s) {
			t.Fatal(s)
		}
	}
}

func TestNamedBindings(t *testing.T) {
	top := strings.Replace(topConfig, "profiles.default", "profiles.custom", 1)
	top = strings.Replace(top, "claude", "opencode", 1)
	for _, role := range roleNames {
		top += "\n[roles." + role + "]\nprofile = 'custom'\n"
	}
	c, err := Load(fixture(t, top, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Roles) != 7 || c.Roles["reviewer"].Profile != "custom" {
		t.Fatalf("bindings: %+v", c.Roles)
	}
}

func TestFilesystemErrors(t *testing.T) {
	for _, name := range []string{"clone file", "root file", "config alias", "dangling clone", "socket file"} {
		t.Run(name, func(t *testing.T) {
			opts := fixture(t, topConfig, projectConfig)
			root := filepath.Join(opts.Home, ".osmia")
			switch name {
			case "clone file":
				write(t, filepath.Join(opts.Home, "clone"), "not a directory")
			case "root file":
				opts.Root = filepath.Join(opts.Home, "file")
				write(t, opts.Root, "not a directory")
			case "config alias":
				path := filepath.Join(root, "config.toml")
				if err := os.Rename(path, filepath.Join(opts.Home, "elsewhere.toml")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(opts.Home, "elsewhere.toml"), path); err != nil {
					t.Fatal(err)
				}
			case "dangling clone":
				if err := os.Symlink(filepath.Join(opts.Home, "missing"), filepath.Join(opts.Home, "clone")); err != nil {
					t.Fatal(err)
				}
			case "socket file":
				write(t, filepath.Join(root, "osmia.sock"), "not a socket")
			}
			if c, err := Load(opts); c != nil || err == nil {
				t.Fatalf("accepted: %+v %v", c, err)
			}
		})
	}
}
