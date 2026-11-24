package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

func str(s string) *string { return &s }
func num(n int) *int       { return &n }
func yes(b bool) *bool     { return &b }

func settingOf(t *testing.T, snapshot ProjectConfigResponse, name string) ProjectConfigSetting {
	t.Helper()
	for _, s := range snapshot.Settings {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no setting %s in %+v", name, snapshot.Settings)
	return ProjectConfigSetting{}
}

func refused(t *testing.T, err error, code Code) *APIError {
	t.Helper()
	var failure *APIError
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("error %v, want code %s", err, code)
	}
	return failure
}

func savedFile(clone string) string {
	return `# Settings of the shop project.
version = 1
upstream = "upstream/repo"
fork = "owner/repo"
# Where the clone lives.
clone = "` + clone + `"
checks_timeout = "15m" # the default, spelled out

[capacity]
# Keep this low.
git_operations = 3
`
}

type configFixture struct {
	s     *Service
	c     *Client
	opts  Options
	files reloadFiles
	text  string
}

func projectConfigFixture(t *testing.T) configFixture {
	t.Helper()
	home, err := os.MkdirTemp("", "os-")
	intakefixture.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	clone := filepath.Join(home, "clone")
	opts := testOptions(fixturekit.ConfigWithSettings(t, home, savedFile(clone)))
	files := configFiles(t, opts)
	s, c := start(t, opts)
	return configFixture{s: s, c: c, opts: opts, files: files, text: savedFile(clone)}
}

func (f configFixture) store(t *testing.T) *projectstore.Store {
	t.Helper()
	root, err := config.ResolveRoot(f.opts.Config.Root, "")
	intakefixture.Must(t, err)
	return projectstore.Open(root)
}

func (f configFixture) stored(t *testing.T) projectstore.Revision {
	t.Helper()
	root, err := config.ResolveRoot(f.opts.Config.Root, "")
	intakefixture.Must(t, err)
	revision, err := projectstore.Open(root).Current(intakefixture.Project)
	intakefixture.Must(t, err)
	return revision
}

func (f configFixture) record(t *testing.T, text string) projectstore.Revision {
	t.Helper()
	revision, err := f.store(t).Replace(intakefixture.Project, f.stored(t).Pin, []byte(text))
	intakefixture.Must(t, err)
	return revision
}

// The read returns every effective setting with its stored value, whether the
// stored settings leave it out, the value in effect and the pin that names the
// stored revision. Only an active project can be read.
func TestProjectConfigReadReturnsTheStoredSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := projectConfigFixture(t)
	s, c, opts := f.s, f.c, f.opts
	got, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	if revision := f.stored(t); got.Project != intakefixture.Project || got.Revision != revision.Number || got.Pin != revision.Pin || got.Error != nil {
		t.Fatalf("snapshot: %+v", got)
	}
	var names []string
	for _, setting := range got.Settings {
		names = append(names, setting.Name)
		if setting.Pending || !reflect.DeepEqual(setting.Value, setting.Effective) || setting.Default == nil {
			t.Fatalf("setting %+v", setting)
		}
	}
	if !reflect.DeepEqual(names, config.ProjectSettings) {
		t.Fatalf("settings %v", names)
	}
	for name, want := range map[string]struct {
		value     any
		isDefault bool
	}{
		"upstream":                {"upstream/repo", false},
		"fork":                    {"owner/repo", false},
		"checks_timeout":          {"15m", false},
		"base_branch":             {"main", true},
		"landing":                 {"commit-per-unit", true},
		"delivery":                {"pull-request", true},
		"name":                    {"", true},
		"upstream_rebase":         {"6h", true},
		"pull_request_poll":       {"10m", true},
		"capacity.git_operations": {float64(3), false},
		"capacity.per_workstream": {float64(s.current().Capacity.PerWorkstream), true},
		"hearsay_scope":           {"", true},
		"hearsay_declare_repo":    {false, true},
		"flake_fixes":             {false, true},
		"hearsay_entities":        {map[string]any{}, true},
	} {
		setting := settingOf(t, got, name)
		if !reflect.DeepEqual(setting.Value, want.value) || *setting.Default != want.isDefault {
			t.Errorf("%s: %+v, want %v default %v", name, setting, want.value, want.isDefault)
		}
	}
	if clone := settingOf(t, got, "clone"); clone.Value != filepath.Join(filepath.Dir(opts.Config.Root), "clone") {
		t.Fatalf("clone: %+v", clone)
	}

	_, err = c.ProjectConfig(ctx, "p_00000000000000000000000000000009")
	refused(t, err, NotFound)
	_, err = c.ProjectConfig(ctx, "not-a-project")
	refused(t, err, Validation)
	_, err = c.SaveProjectConfig(ctx, "p_00000000000000000000000000000009", ProjectConfigEdit{Pin: got.Pin})
	refused(t, err, NotFound)
}

func TestProjectConfigSaveBuildsOnAnUnappliedRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := projectConfigFixture(t)
	s, c := f.s, f.c
	edited := strings.Replace(f.text, `checks_timeout = "15m"`, `checks_timeout = "30m"`, 1)
	revision := f.record(t, edited)
	intakefixture.Must(t, os.WriteFile(f.files.top, []byte(f.files.topText+"[capacity]\nmasons = 7\n"), 0600))

	got, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	timeout := settingOf(t, got, "checks_timeout")
	if timeout.Value != "30m" || timeout.Effective != "15m" || !timeout.Pending || got.Pin != revision.Pin || got.Revision != revision.Number {
		t.Fatalf("unapplied revision: %+v pin %s", timeout, got.Pin)
	}
	for _, setting := range got.Settings {
		if setting.Pending != (setting.Name == "checks_timeout") {
			t.Fatalf("only checks_timeout is pending: %+v", setting)
		}
	}

	saved, err := c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.Landing = str("squash") })})
	intakefixture.Must(t, err)
	want := strings.Replace(edited, "checks_timeout = \"30m\" # the default, spelled out\n", "checks_timeout = \"30m\" # the default, spelled out\nlanding = \"squash\"\n", 1)
	now := f.stored(t)
	if string(now.Data) != want || saved.Outcome != ProjectConfigApplied || saved.Pin != now.Pin || saved.Revision != revision.Number+1 {
		t.Fatalf("saved %+v\nstored:\n%s\nwant:\n%s", saved, now.Data, want)
	}
	if p := s.current().For(intakefixture.Project).Project; p.ChecksTimeout != "30m" || p.Landing != "squash" {
		t.Fatalf("in effect: %+v", p)
	}
	if s.current().Capacity.Masons == 7 {
		t.Fatal("the save applied the top-level file")
	}
	view, err := c.Configuration(ctx)
	intakefixture.Must(t, err)
	if view.Effective.Project.ChecksTimeout != "30m" || len(view.Drift.Files) != 1 || view.Drift.Files[0].State != ConfigChanged {
		t.Fatalf("configuration after the save: %+v", view.Drift)
	}
	for _, setting := range saved.Settings {
		if setting.Pending {
			t.Fatalf("applied save left %+v pending", setting)
		}
	}
}

// A save changes the edited keys and no other byte, announces the change, and
// the values are what the next start loads from the store.
func TestProjectConfigSavePreservesUntouchedBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := projectConfigFixture(t)
	s, c := f.s, f.c
	events, _ := fixturekit.EventStream(t, c)
	fixturekit.NextEvent(t, events)
	got, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)

	saved, err := c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) {
		v.ChecksTimeout = str("45m")
		v.CapacityPerWorkstream = num(5)
		v.CapacityGitOperations = num(2)
		v.HearsayScope = str("work:main:shop")
		v.HearsayDeclareRepo = yes(true)
		v.HearsayEntities = map[string]string{"cart": "code:shop:cart"}
		v.Name = str("Shop")
	})})
	intakefixture.Must(t, err)
	fixturekit.AwaitEvents(t, events, Event{Kind: EventConfig})
	want := `# Settings of the shop project.
version = 1
upstream = "upstream/repo"
fork = "owner/repo"
# Where the clone lives.
clone = "` + settingOf(t, got, "clone").Value.(string) + `"
checks_timeout = "45m" # the default, spelled out
hearsay_scope = "work:main:shop"
name = "Shop"
hearsay_declare_repo = true

[capacity]
# Keep this low.
git_operations = 2
per_workstream = 5

[hearsay_entities]
cart = "code:shop:cart"
`
	stored := f.stored(t)
	if string(stored.Data) != want || saved.Outcome != ProjectConfigApplied {
		t.Fatalf("stored:\n%s\nwant:\n%s\noutcome %+v", stored.Data, want, saved)
	}
	p := s.current().For(intakefixture.Project).Project
	if p.ChecksTimeout != "45m" || p.Capacity.PerWorkstream != 5 || p.Capacity.GitOperations != 2 || p.HearsayScope != "work:main:shop" || !p.HearsayDeclareRepo || p.Name != "Shop" || !reflect.DeepEqual(p.HearsayEntities, map[string]string{"cart": "code:shop:cart"}) {
		t.Fatalf("in effect: %+v", p)
	}
	root, err := config.ResolveRoot(f.opts.Config.Root, "")
	intakefixture.Must(t, err)
	top, err := config.LoadTopLevel(f.opts.Config)
	intakefixture.Must(t, err)
	if loaded, _, err := projectstore.Open(root).Load(top, f.opts.Config.Home, intakefixture.Project); err != nil || !reflect.DeepEqual(loaded.Project, p) {
		t.Fatalf("a fresh load of the store differs: %v\n%+v\n%+v", err, loaded.Project, p)
	}
	view, err := c.Configuration(ctx)
	intakefixture.Must(t, err)
	if view.Drift.Differs || !reflect.DeepEqual(view.Effective.Project, p) {
		t.Fatalf("configuration: %+v", view.Drift)
	}
	again, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	if !reflect.DeepEqual(again, saved.ProjectConfigResponse) {
		t.Fatalf("snapshot after the save differs:\n%+v\n%+v", again, saved.ProjectConfigResponse)
	}
}

// flake_fixes is off until a pinned save turns it on; the running service
// adopts the value, and clearing the key returns to the default.
func TestProjectConfigSaveTurnsFlakeFixesOnAndOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := projectConfigFixture(t)
	s, c := f.s, f.c
	got, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	if setting := settingOf(t, got, "flake_fixes"); setting.Value != false || setting.Effective != false || setting.Default == nil || !*setting.Default || setting.Pending {
		t.Fatalf("before: %+v", setting)
	}
	if s.current().For(intakefixture.Project).Project.FlakeFixes {
		t.Fatal("flake_fixes is not off by default")
	}

	on, err := c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.FlakeFixes = yes(true) })})
	intakefixture.Must(t, err)
	if on.Outcome != ProjectConfigApplied || !strings.Contains(string(f.stored(t).Data), "flake_fixes = true\n") {
		t.Fatalf("on: %+v\n%s", on, f.stored(t).Data)
	}
	if setting := settingOf(t, on.ProjectConfigResponse, "flake_fixes"); setting.Value != true || setting.Effective != true || *setting.Default || setting.Pending {
		t.Fatalf("after turning on: %+v", setting)
	}
	if !s.current().For(intakefixture.Project).Project.FlakeFixes {
		t.Fatal("the running service did not adopt flake_fixes = true")
	}

	off, err := c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: on.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.FlakeFixes = yes(false) })})
	intakefixture.Must(t, err)
	if setting := settingOf(t, off.ProjectConfigResponse, "flake_fixes"); setting.Value != false || setting.Effective != false || *setting.Default || setting.Pending {
		t.Fatalf("after turning off: %+v", setting)
	}
	if s.current().For(intakefixture.Project).Project.FlakeFixes {
		t.Fatal("the running service did not adopt flake_fixes = false")
	}

	cleared, err := c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: off.Pin, Clear: []string{"flake_fixes"}})
	intakefixture.Must(t, err)
	if strings.Contains(string(f.stored(t).Data), "flake_fixes") || !*settingOf(t, cleared.ProjectConfigResponse, "flake_fixes").Default {
		t.Fatalf("cleared:\n%s", f.stored(t).Data)
	}
}

// Clearing a defaulted or optional setting removes its key and the default or
// inherited value applies again.
func TestProjectConfigClearRemovesKeysAndRestoresDefaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := projectConfigFixture(t)
	s, c := f.s, f.c
	more := strings.Replace(f.text, "\n\n[capacity]", "\nlanding = \"squash\"\n\n[capacity]", 1)
	more = strings.Replace(more, "[capacity]\n", "[capacity]\nper_workstream = 5\n", 1) + "\n[hearsay_entities]\na = \"code:a\"\nb = \"code:b\"\n"
	read, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	revision, err := f.store(t).Replace(intakefixture.Project, read.Pin, []byte(more))
	intakefixture.Must(t, err)
	_, err = c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: revision.Pin})
	intakefixture.Must(t, err)
	got, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	if p := s.current().For(intakefixture.Project).Project; p.Landing != "squash" || p.Capacity.PerWorkstream != 5 {
		t.Fatalf("before: %+v", p)
	}

	saved, err := c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: got.Pin, Clear: []string{"landing", "checks_timeout", "capacity.per_workstream"}, ClearHearsayEntities: []string{"a"}})
	intakefixture.Must(t, err)
	stored := string(f.stored(t).Data)
	for _, gone := range []string{"landing", "checks_timeout", "per_workstream", "a = "} {
		if strings.Contains(stored, gone) {
			t.Fatalf("%s is still in the stored settings:\n%s", gone, stored)
		}
	}
	for _, kept := range []string{"# Keep this low.", "git_operations = 3", "b = \"code:b\""} {
		if !strings.Contains(stored, kept) {
			t.Fatalf("%s is missing from the stored settings:\n%s", kept, stored)
		}
	}
	p := s.current().For(intakefixture.Project).Project
	if p.Landing != "commit-per-unit" || p.ChecksTimeout != "15m" || p.Capacity.PerWorkstream != s.current().Capacity.PerWorkstream || !reflect.DeepEqual(p.HearsayEntities, map[string]string{"b": "code:b"}) {
		t.Fatalf("defaults after clearing: %+v", p)
	}
	for _, name := range []string{"landing", "checks_timeout", "capacity.per_workstream"} {
		if setting := settingOf(t, saved.ProjectConfigResponse, name); !*setting.Default || setting.Pending {
			t.Fatalf("%s: %+v", name, setting)
		}
	}
	for _, name := range []string{"upstream", "clone"} {
		_, err = c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: saved.Pin, Clear: []string{name}})
		if failure := refused(t, err, Validation); failure.Field != name {
			t.Fatalf("clearing %s: %+v", name, failure)
		}
	}
}

// An invalid value is refused with a field error naming the field, and the
// stored settings and the loaded configuration stay as they were.
func TestProjectConfigSaveRefusesInvalidValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := projectConfigFixture(t)
	s, c := f.s, f.c
	got, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	before := s.current()
	for _, tc := range []struct {
		name, field string
		set         func(*ProjectConfigEditValues)
	}{
		{"timeout above the bound", "checks_timeout", func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("25h") }},
		{"timeout below the bound", "checks_timeout", func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("30s") }},
		{"malformed upstream", "upstream", func(v *ProjectConfigEditValues) { v.Upstream = str("https://github.com/a/b.git") }},
		{"malformed fork", "fork", func(v *ProjectConfigEditValues) { v.Fork = str("nope") }},
		{"branch", "base_branch", func(v *ProjectConfigEditValues) { v.BaseBranch = str("a..b") }},
		{"landing", "landing", func(v *ProjectConfigEditValues) { v.Landing = str("sideways") }},
		{"delivery", "delivery", func(v *ProjectConfigEditValues) { v.Delivery = str("merge") }},
		{"delivery jev on a fork project", "delivery", func(v *ProjectConfigEditValues) { v.Delivery = str("jev") }},
		{"capacity", "capacity.per_workstream", func(v *ProjectConfigEditValues) { v.CapacityPerWorkstream = num(0) }},
		{"entity", "hearsay_entities", func(v *ProjectConfigEditValues) { v.HearsayEntities = map[string]string{"shop": "NOT VALID"} }},
		{"old scope name as entity", "hearsay_entities", func(v *ProjectConfigEditValues) { v.HearsayEntities = map[string]string{"shop": "osmia-ab12"} }},
		{"old scope name", "hearsay_scope", func(v *ProjectConfigEditValues) { v.HearsayScope = str("osmia-ab12") }},
	} {
		_, err := c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: got.Pin, Set: changes(tc.set)})
		failure := refused(t, err, Validation)
		if failure.Field != tc.field || !strings.Contains(failure.Message, tc.field) || !strings.Contains(failure.Message, config.ProjectSettingsLabel(intakefixture.Project)+": "+tc.field) || strings.Contains(failure.Message, "config.toml") {
			t.Errorf("%s: %+v", tc.name, failure)
		}
		if f.stored(t).Pin != got.Pin || s.current() != before {
			t.Fatalf("%s changed the stored settings or the loaded configuration", tc.name)
		}
	}
	_, err = c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: got.Pin, Clear: []string{"nonsense"}})
	refused(t, err, Validation)
	_, err = c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Set: changes(func(v *ProjectConfigEditValues) { v.Landing = str("squash") })})
	if failure := refused(t, err, Validation); failure.Field != "pin" {
		t.Fatalf("no pin: %+v", failure)
	}
	if f.stored(t).Pin != got.Pin || s.current() != before {
		t.Fatal("a refused request changed the stored settings or the loaded configuration")
	}
}

func TestProjectConfigSaveRefusesAStalePin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := projectConfigFixture(t)
	s, c := f.s, f.c
	got, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	edited := strings.Replace(f.text, `checks_timeout = "15m"`, `checks_timeout = "20m"`, 1)
	revision := f.record(t, edited)
	before := s.current()

	_, err = c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.Landing = str("squash") })})
	refused(t, err, Conflict)
	if f.stored(t).Pin != revision.Pin || s.current() != before {
		t.Fatal("a stale save wrote or applied something")
	}
	if revisions, err := f.store(t).Revisions(intakefixture.Project); err != nil || len(revisions) != 2 {
		t.Fatalf("revisions after a stale save: %d %v", len(revisions), err)
	}
	fresh, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	if fresh.Pin != revision.Pin || settingOf(t, fresh, "checks_timeout").Value != "20m" {
		t.Fatalf("fresh read: %+v", fresh)
	}
	saved, err := c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: fresh.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.Landing = str("squash") })})
	intakefixture.Must(t, err)
	if saved.Outcome != ProjectConfigApplied || s.current().For(intakefixture.Project).Project.ChecksTimeout != "20m" {
		t.Fatalf("pinned save: %+v", saved)
	}
}

func TestProjectConfigReadReportsSettingsThatDoNotLoad(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := projectConfigFixture(t)
	s, c := f.s, f.c
	good, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	before := s.current()

	broken := f.record(t, strings.Replace(f.text, "version = 1\n", "version = 1\nlanding = \"sideways\"\n", 1))
	got, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	label := config.ProjectSettingsLabel(intakefixture.Project)
	if got.Pin != "" || got.Error == nil || got.Error.Field != "landing" || !strings.Contains(got.Error.Message, label+": landing: ") || strings.Contains(got.Error.Message, "config.toml") {
		t.Fatalf("snapshot of broken settings: %+v", got)
	}
	for _, setting := range got.Settings {
		if setting.Value != nil || setting.Default != nil || setting.Pending || setting.Effective == nil {
			t.Fatalf("broken settings show only the values in effect: %+v", setting)
		}
	}
	if landing := settingOf(t, got, "landing"); landing.Effective != "commit-per-unit" {
		t.Fatalf("landing in effect: %+v", landing)
	}
	for _, pin := range []string{"", good.Pin, broken.Pin} {
		_, err = c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: pin, Set: changes(func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("20m") })})
		if err == nil {
			t.Fatalf("a save with pin %q succeeded on settings that do not load", pin)
		}
		if pin == broken.Pin {
			failure := refused(t, err, Validation)
			for _, want := range []string{label + ": landing: ", "osmia project settings " + string(intakefixture.Project) + " set landing=VALUE", "service is stopped"} {
				if !strings.Contains(failure.Message, want) {
					t.Fatalf("refusal lacks %q: %s", want, failure.Message)
				}
			}
			if strings.Contains(failure.Message, "config.toml") || strings.Contains(failure.Message, "by hand") {
				t.Fatalf("refusal points at a file: %s", failure.Message)
			}
		}
	}
	if f.stored(t).Pin != broken.Pin || s.current() != before {
		t.Fatal("a refused save changed the stored settings or the loaded configuration")
	}
	f.record(t, "version = 1\nsecret = 'shh\n[")
	got, err = c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	if got.Pin != "" || got.Error == nil || strings.Contains(got.Error.Message, "shh") {
		t.Fatalf("snapshot of unparsable settings: %+v", got)
	}

	f.record(t, f.text)
	got, err = c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	if got.Error != nil || got.Pin == "" {
		t.Fatalf("snapshot once the settings load: %+v", got)
	}
	_, err = c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("20m") })})
	intakefixture.Must(t, err)
}

// A failure preparing the running service's adoption, or writing the revision,
// refuses the save with the reason and leaves the stored settings and the
// loaded configuration as they were.
func TestProjectConfigSaveRefusesWhenPreparingOrWritingFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := projectConfigFixture(t)
	s, c := f.s, f.c
	got, err := c.ProjectConfig(ctx, intakefixture.Project)
	intakefixture.Must(t, err)
	edit := ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("20m") })}
	before := s.current()
	unchanged := func(step string) {
		t.Helper()
		revisions, err := f.store(t).Revisions(intakefixture.Project)
		intakefixture.Must(t, err)
		if len(revisions) != 1 || f.stored(t).Pin != got.Pin || s.current() != before {
			t.Fatalf("%s left a revision recorded or the loaded configuration changed", step)
		}
	}

	s.setBoundary(func(step string) error {
		if step == "project-config-prepare" {
			return errors.New("engine is not reachable")
		}
		return nil
	})
	_, err = c.SaveProjectConfig(ctx, intakefixture.Project, edit)
	if failure := refused(t, err, Internal); !strings.Contains(failure.Message, "engine is not reachable") || !strings.Contains(failure.Message, "unchanged") {
		t.Fatalf("preparation failure: %+v", failure)
	}
	unchanged("a preparation failure")

	s.setBoundary(func(step string) error {
		if strings.HasPrefix(step, "revision-staged:") {
			return errors.New("disk full")
		}
		return nil
	})
	_, err = c.SaveProjectConfig(ctx, intakefixture.Project, edit)
	if failure := refused(t, err, Internal); !strings.Contains(failure.Message, "disk full") || !strings.Contains(failure.Message, string(intakefixture.Project)) {
		t.Fatalf("write failure: %+v", failure)
	}
	unchanged("a write failure")

	s.setBoundary(func(step string) error {
		if step == "project-config-write" {
			return errors.New("interrupted")
		}
		return nil
	})
	_, err = c.SaveProjectConfig(ctx, intakefixture.Project, edit)
	refused(t, err, Internal)
	unchanged("an interrupted write")

	s.setBoundary(nil)
	saved, err := c.SaveProjectConfig(ctx, intakefixture.Project, edit)
	intakefixture.Must(t, err)
	if saved.Outcome != ProjectConfigApplied || s.current().For(intakefixture.Project).Project.ChecksTimeout != "20m" {
		t.Fatalf("save after the failures: %+v", saved)
	}
}

func TestProjectConfigSaveReportsAFailureAfterTheWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, failAt := range []string{"project-config-written", "revision-recorded"} {
		t.Run(failAt, func(t *testing.T) {
			t.Parallel()
			f := projectConfigFixture(t)
			s, c := f.s, f.c
			got, err := c.ProjectConfig(ctx, intakefixture.Project)
			intakefixture.Must(t, err)
			s.setBoundary(func(step string) error {
				if strings.HasPrefix(step, failAt) {
					return errors.New("cannot reach the scheduler")
				}
				return nil
			})
			saved, err := c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("20m") })})
			intakefixture.Must(t, err)
			stored := f.stored(t)
			if saved.Outcome != ProjectConfigSavedNotApplied || !strings.Contains(saved.Reason, "cannot reach the scheduler") || saved.Pin != stored.Pin || !strings.Contains(string(stored.Data), `checks_timeout = "20m"`) {
				t.Fatalf("saved %+v\n%s", saved, stored.Data)
			}
			if timeout := settingOf(t, saved.ProjectConfigResponse, "checks_timeout"); timeout.Value != "20m" || timeout.Effective != "15m" || !timeout.Pending {
				t.Fatalf("saved values read as applied: %+v", timeout)
			}
			if s.current().For(intakefixture.Project).Project.ChecksTimeout != "15m" {
				t.Fatal("the failed save was applied")
			}
			read, err := c.ProjectConfig(ctx, intakefixture.Project)
			intakefixture.Must(t, err)
			if read.Pin != saved.Pin || !settingOf(t, read, "checks_timeout").Pending {
				t.Fatalf("read after: %+v", read)
			}

			s.setBoundary(nil)
			intakefixture.Must(t, os.WriteFile(f.files.top, []byte(f.files.topText+"[capacity]\nmasons = 7\n"), 0600))
			_, err = c.Reload(ctx)
			intakefixture.Must(t, err)
			if p := s.current().For(intakefixture.Project).Project; p.ChecksTimeout != "15m" || s.current().Capacity.Masons != 7 {
				t.Fatalf("after the reload: %+v masons %d", p, s.current().Capacity.Masons)
			}
			read, err = c.ProjectConfig(ctx, intakefixture.Project)
			intakefixture.Must(t, err)
			if read.Pin != saved.Pin || !settingOf(t, read, "checks_timeout").Pending {
				t.Fatalf("the reload did not keep the revision pending: %+v", read)
			}

			applied, err := c.SaveProjectConfig(ctx, intakefixture.Project, ProjectConfigEdit{Pin: saved.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.Landing = str("squash") })})
			intakefixture.Must(t, err)
			if applied.Outcome != ProjectConfigApplied {
				t.Fatalf("later save: %+v", applied)
			}
			if p := s.current().For(intakefixture.Project).Project; p.ChecksTimeout != "20m" || p.Landing != "squash" {
				t.Fatalf("in effect: %+v", p)
			}
		})
	}
}

// The endpoints follow the web listener's rules: a JSON content type for a
// save, and a Host naming the listener.
func TestProjectConfigEndpointsFollowTheWebListenerRules(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "web = \"127.0.0.1:0\"\n")
	s, c := start(t, opts)
	got, err := c.ProjectConfig(context.Background(), intakefixture.Project)
	intakefixture.Must(t, err)
	url := "http://" + s.WebAddr() + Prefix + "/projects/config/" + string(intakefixture.Project)
	body := `{"pin":"` + got.Pin + `","set":{"landing":"squash"}}`
	if code, out := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", url, "", "", ""); code != 200 || !strings.Contains(string(out), got.Pin) {
		t.Fatalf("read: %d %s", code, out)
	}
	if code, _ := intakefixture.Exchange(t, intakefixture.WebHTTP(), "GET", url, "", "", "evil.example"); code != 403 {
		t.Fatalf("read with a foreign Host: %d", code)
	}
	for _, contentType := range []string{"", "text/plain"} {
		if code, _ := intakefixture.Exchange(t, intakefixture.WebHTTP(), "PUT", url, contentType, body, ""); code != 403 {
			t.Fatalf("save with content type %q: %d", contentType, code)
		}
	}
	if code, _ := intakefixture.Exchange(t, intakefixture.WebHTTP(), "PUT", url, "application/json", body, "evil.example"); code != 403 {
		t.Fatalf("save with a foreign Host: %d", code)
	}
	if code, out := intakefixture.Exchange(t, intakefixture.WebHTTP(), "PUT", url, "application/json", body, ""); code != 200 || !strings.Contains(string(out), `"outcome":"applied"`) {
		t.Fatalf("save: %d %s", code, out)
	}
	if code, _ := intakefixture.Exchange(t, intakefixture.WebHTTP(), "DELETE", url, "application/json", "", ""); code != 501 {
		t.Fatalf("unsupported method: %d", code)
	}
}

func changes(set func(*ProjectConfigEditValues)) ProjectConfigEditValues {
	var v ProjectConfigEditValues
	set(&v)
	return v
}
