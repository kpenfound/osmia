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
)

func str(s string) *string { return &s }
func num(n int) *int       { return &n }
func yes(b bool) *bool     { return &b }

// settingOf returns the named setting of a snapshot.
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

// savedFile is a project file as an owner keeps it: commented, reordered,
// with an ignored [preview] table.
func savedFile(clone string) string {
	return `# Settings of the shop project.
version = 1
upstream = "upstream/repo"
fork = "owner/repo"
# Where the clone lives.
clone = "` + clone + `"
checks_timeout = "15m" # the default, spelled out

[preview]
call = ["serve"]
port = 8080

[capacity]
# Keep this low.
git_operations = 3
`
}

// projectConfigFixture starts a service whose project file is savedFile.
func projectConfigFixture(t *testing.T) (*Service, *Client, Options, reloadFiles) {
	t.Helper()
	opts := fixture(t)
	files := configFiles(t, opts)
	clone := filepath.Join(filepath.Dir(opts.Config.Root), "clone")
	files.write(t, files.topText, savedFile(clone))
	files.projectText = savedFile(clone)
	s, c := start(t, opts)
	return s, c, opts, files
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	return string(data)
}

// The read returns every effective setting with the file's value, whether the
// file leaves it out, the value in effect and the pin of the bytes it decoded.
// Only an active project can be read.
func TestProjectConfigReadReturnsTheFileSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, opts, files := projectConfigFixture(t)
	got, err := c.ProjectConfig(ctx, project)
	must(t, err)
	if got.Project != project || got.Path != files.project || got.Pin != config.ProjectPin([]byte(files.projectText)) || got.Error != nil {
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
		"classifier":              {"", true},
		"name":                    {"", true},
		"upstream_rebase":         {"6h", true},
		"pull_request_poll":       {"10m", true},
		"capacity.git_operations": {float64(3), false},
		// The top-level value is inherited.
		"capacity.per_workstream": {float64(s.current().Capacity.PerWorkstream), true},
		"hearsay_scope":           {"", true},
		"hearsay_declare_repo":    {false, true},
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

// A hand edit the service has not applied shows as the file's value beside the
// value in effect, with the edited file's pin. A save of another setting with
// that pin keeps the hand edit in the file and applies it, and applies
// nothing else that is waiting on disk.
func TestProjectConfigSaveKeepsAnUnappliedHandEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, opts, files := projectConfigFixture(t)
	edited := strings.Replace(files.projectText, `checks_timeout = "15m"`, `checks_timeout = "30m"`, 1)
	files.write(t, files.topText+"[capacity]\nmasons = 7\n", edited)

	got, err := c.ProjectConfig(ctx, project)
	must(t, err)
	timeout := settingOf(t, got, "checks_timeout")
	if timeout.Value != "30m" || timeout.Effective != "15m" || !timeout.Pending || got.Pin != config.ProjectPin([]byte(edited)) {
		t.Fatalf("hand edit: %+v pin %s", timeout, got.Pin)
	}
	for _, setting := range got.Settings {
		if setting.Pending != (setting.Name == "checks_timeout") {
			t.Fatalf("only checks_timeout is pending: %+v", setting)
		}
	}

	saved, err := c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.Landing = str("squash") })})
	must(t, err)
	want := strings.Replace(edited, "checks_timeout = \"30m\" # the default, spelled out\n", "checks_timeout = \"30m\" # the default, spelled out\nlanding = \"squash\"\n", 1)
	disk := readFile(t, files.project)
	if disk != want || saved.Outcome != ProjectConfigApplied || saved.Pin != config.ProjectPin([]byte(disk)) {
		t.Fatalf("saved %+v\nfile:\n%s\nwant:\n%s", saved, disk, want)
	}
	if p := s.current().For(project).Project; p.ChecksTimeout != "30m" || p.Landing != "squash" {
		t.Fatalf("in effect: %+v", p)
	}
	// Another file's pending edit stays unapplied.
	if s.current().Capacity.Masons == 7 {
		t.Fatal("the save applied the top-level file")
	}
	view, err := c.Configuration(ctx)
	must(t, err)
	drift := view.Drift.Files
	if view.Effective.Project.ChecksTimeout != "30m" || drift[1].State != ConfigUnchanged || drift[0].State != ConfigChanged {
		t.Fatalf("configuration after the save: %+v", view.Drift)
	}
	for _, setting := range saved.Settings {
		if setting.Pending {
			t.Fatalf("applied save left %+v pending", setting)
		}
	}
	fresh, err := config.Load(opts.Config)
	must(t, err)
	if !reflect.DeepEqual(fresh.Project, s.current().For(project).Project) {
		t.Fatalf("a fresh load differs:\n%+v\n%+v", fresh.Project, s.current().For(project).Project)
	}
}

// A save changes the edited keys and no other byte, announces the change, and
// the values are what a fresh load of the root reads.
func TestProjectConfigSavePreservesUntouchedBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, opts, files := projectConfigFixture(t)
	events, _ := eventStream(t, c)
	nextEvent(t, events)
	got, err := c.ProjectConfig(ctx, project)
	must(t, err)

	saved, err := c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) {
		v.ChecksTimeout = str("45m")
		v.Classifier = str("other")
		v.CapacityPerWorkstream = num(5)
		v.CapacityGitOperations = num(2)
		v.HearsayScope = str("work:main:shop")
		v.HearsayDeclareRepo = yes(true)
		v.HearsayEntities = map[string]string{"cart": "code:shop:cart"}
		v.Name = str("Shop")
	})})
	must(t, err)
	awaitEvents(t, events, Event{Kind: EventConfig})
	want := `# Settings of the shop project.
version = 1
upstream = "upstream/repo"
fork = "owner/repo"
# Where the clone lives.
clone = "` + settingOf(t, got, "clone").Value.(string) + `"
checks_timeout = "45m" # the default, spelled out
classifier = "other"
hearsay_scope = "work:main:shop"
name = "Shop"
hearsay_declare_repo = true

[preview]
call = ["serve"]
port = 8080

[capacity]
# Keep this low.
git_operations = 2
per_workstream = 5

[hearsay_entities]
cart = "code:shop:cart"
`
	// New root keys follow the last root key, so they sit before [preview].
	disk := readFile(t, files.project)
	if disk != want || saved.Outcome != ProjectConfigApplied {
		t.Fatalf("file:\n%s\nwant:\n%s\noutcome %+v", disk, want, saved)
	}
	p := s.current().For(project).Project
	if p.ChecksTimeout != "45m" || p.Classifier != "other" || p.Capacity.PerWorkstream != 5 || p.Capacity.GitOperations != 2 || p.HearsayScope != "work:main:shop" || !p.HearsayDeclareRepo || p.Name != "Shop" || !reflect.DeepEqual(p.HearsayEntities, map[string]string{"cart": "code:shop:cart"}) {
		t.Fatalf("in effect: %+v", p)
	}
	fresh, err := config.Load(opts.Config)
	must(t, err)
	if !reflect.DeepEqual(fresh.Project, p) {
		t.Fatalf("a fresh load differs:\n%+v\n%+v", fresh.Project, p)
	}
	view, err := c.Configuration(ctx)
	must(t, err)
	if view.Drift.Differs || !reflect.DeepEqual(view.Effective.Project, p) {
		t.Fatalf("configuration: %+v", view.Drift)
	}
	// The saved pin reads back as the same snapshot.
	again, err := c.ProjectConfig(ctx, project)
	must(t, err)
	if !reflect.DeepEqual(again, saved.ProjectConfigResponse) {
		t.Fatalf("snapshot after the save differs:\n%+v\n%+v", again, saved.ProjectConfigResponse)
	}
}

// Clearing a defaulted or optional setting removes its key and the default or
// inherited value applies again.
func TestProjectConfigClearRemovesKeysAndRestoresDefaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, _, files := projectConfigFixture(t)
	more := strings.Replace(files.projectText, "\n\n[preview]", "\nclassifier = \"other\"\nlanding = \"squash\"\n\n[preview]", 1)
	more = strings.Replace(more, "[capacity]\n", "[capacity]\nper_workstream = 5\n", 1) + "\n[hearsay_entities]\na = \"code:a\"\nb = \"code:b\"\n"
	files.write(t, files.topText, more)
	_, err := c.Reload(ctx)
	must(t, err)
	got, err := c.ProjectConfig(ctx, project)
	must(t, err)
	if p := s.current().For(project).Project; p.Classifier != "other" || p.Capacity.PerWorkstream != 5 {
		t.Fatalf("before: %+v", p)
	}

	saved, err := c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: got.Pin, Clear: []string{"classifier", "landing", "checks_timeout", "capacity.per_workstream"}, ClearHearsayEntities: []string{"a"}})
	must(t, err)
	disk := readFile(t, files.project)
	for _, gone := range []string{"classifier", "landing", "checks_timeout", "per_workstream", "a = "} {
		if strings.Contains(disk, gone) {
			t.Fatalf("%s is still in the file:\n%s", gone, disk)
		}
	}
	for _, kept := range []string{"# Keep this low.", "git_operations = 3", "[preview]", "b = \"code:b\""} {
		if !strings.Contains(disk, kept) {
			t.Fatalf("%s is missing from the file:\n%s", kept, disk)
		}
	}
	p := s.current().For(project).Project
	if p.Classifier != "" || p.Landing != "commit-per-unit" || p.ChecksTimeout != "15m" || p.Capacity.PerWorkstream != s.current().Capacity.PerWorkstream || !reflect.DeepEqual(p.HearsayEntities, map[string]string{"b": "code:b"}) {
		t.Fatalf("defaults after clearing: %+v", p)
	}
	for _, name := range []string{"classifier", "landing", "checks_timeout", "capacity.per_workstream"} {
		if setting := settingOf(t, saved.ProjectConfigResponse, name); !*setting.Default || setting.Pending {
			t.Fatalf("%s: %+v", name, setting)
		}
	}
	for _, name := range []string{"upstream", "clone"} {
		_, err = c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: saved.Pin, Clear: []string{name}})
		if failure := refused(t, err, Validation); failure.Field != name {
			t.Fatalf("clearing %s: %+v", name, failure)
		}
	}
}

// An invalid value is refused with a field error naming the field, and the
// file and the loaded configuration stay as they were.
func TestProjectConfigSaveRefusesInvalidValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, _, files := projectConfigFixture(t)
	got, err := c.ProjectConfig(ctx, project)
	must(t, err)
	before := s.current()
	for _, tc := range []struct {
		name, field string
		set         func(*ProjectConfigEditValues)
	}{
		{"timeout above the bound", "checks_timeout", func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("25h") }},
		{"timeout below the bound", "checks_timeout", func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("30s") }},
		{"unknown classifier", "classifier", func(v *ProjectConfigEditValues) { v.Classifier = str("missing") }},
		{"malformed upstream", "upstream", func(v *ProjectConfigEditValues) { v.Upstream = str("https://github.com/a/b.git") }},
		{"malformed fork", "fork", func(v *ProjectConfigEditValues) { v.Fork = str("nope") }},
		{"branch", "base_branch", func(v *ProjectConfigEditValues) { v.BaseBranch = str("a..b") }},
		{"landing", "landing", func(v *ProjectConfigEditValues) { v.Landing = str("sideways") }},
		{"capacity", "capacity.per_workstream", func(v *ProjectConfigEditValues) { v.CapacityPerWorkstream = num(0) }},
		{"entity", "hearsay_entities", func(v *ProjectConfigEditValues) { v.HearsayEntities = map[string]string{"shop": "NOT VALID"} }},
		{"old scope name as entity", "hearsay_entities", func(v *ProjectConfigEditValues) { v.HearsayEntities = map[string]string{"shop": "osmia-ab12"} }},
		{"old scope name", "hearsay_scope", func(v *ProjectConfigEditValues) { v.HearsayScope = str("osmia-ab12") }},
	} {
		_, err := c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: got.Pin, Set: changes(tc.set)})
		failure := refused(t, err, Validation)
		if failure.Field != tc.field || !strings.Contains(failure.Message, tc.field) || !strings.Contains(failure.Message, files.project) {
			t.Errorf("%s: %+v", tc.name, failure)
		}
		if readFile(t, files.project) != files.projectText || s.current() != before {
			t.Fatalf("%s changed the file or the loaded configuration", tc.name)
		}
	}
	// A request that sets and clears one setting, or names none, is refused too.
	_, err = c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: got.Pin, Clear: []string{"nonsense"}})
	refused(t, err, Validation)
	_, err = c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Set: changes(func(v *ProjectConfigEditValues) { v.Landing = str("squash") })})
	if failure := refused(t, err, Validation); failure.Field != "pin" {
		t.Fatalf("no pin: %+v", failure)
	}
	if readFile(t, files.project) != files.projectText || s.current() != before {
		t.Fatal("a refused request changed the file or the loaded configuration")
	}
}

// A save pinned to bytes that are no longer on disk is refused as a conflict
// and writes nothing; reading again returns the edited file's values and pin.
func TestProjectConfigSaveRefusesAStalePin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, _, files := projectConfigFixture(t)
	got, err := c.ProjectConfig(ctx, project)
	must(t, err)
	edited := strings.Replace(files.projectText, `checks_timeout = "15m"`, `checks_timeout = "20m"`, 1)
	files.write(t, files.topText, edited)
	before := s.current()

	_, err = c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.Landing = str("squash") })})
	refused(t, err, Conflict)
	if readFile(t, files.project) != edited || s.current() != before {
		t.Fatal("a stale save wrote or applied something")
	}
	fresh, err := c.ProjectConfig(ctx, project)
	must(t, err)
	if fresh.Pin != config.ProjectPin([]byte(edited)) || settingOf(t, fresh, "checks_timeout").Value != "20m" {
		t.Fatalf("fresh read: %+v", fresh)
	}
	// The same save pinned to the fresh read goes through.
	saved, err := c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: fresh.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.Landing = str("squash") })})
	must(t, err)
	if saved.Outcome != ProjectConfigApplied || s.current().For(project).Project.ChecksTimeout != "20m" {
		t.Fatalf("pinned save: %+v", saved)
	}
}

// A file that does not load is reported with the loader's error, the values in
// effect and no pin; no save succeeds until it loads again.
func TestProjectConfigReadReportsAFileThatDoesNotLoad(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, _, files := projectConfigFixture(t)
	good, err := c.ProjectConfig(ctx, project)
	must(t, err)
	before := s.current()

	broken := strings.Replace(files.projectText, "version = 1\n", "version = 1\nlanding = \"sideways\"\n", 1)
	files.write(t, files.topText, broken)
	got, err := c.ProjectConfig(ctx, project)
	must(t, err)
	if got.Pin != "" || got.Error == nil || got.Error.Field != "landing" || !strings.Contains(got.Error.Message, files.project+": landing: ") {
		t.Fatalf("snapshot of a broken file: %+v", got)
	}
	for _, setting := range got.Settings {
		if setting.Value != nil || setting.Default != nil || setting.Pending || setting.Effective == nil {
			t.Fatalf("a broken file shows only the values in effect: %+v", setting)
		}
	}
	if landing := settingOf(t, got, "landing"); landing.Effective != "commit-per-unit" {
		t.Fatalf("landing in effect: %+v", landing)
	}
	for _, pin := range []string{"", good.Pin, config.ProjectPin([]byte(broken))} {
		_, err = c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: pin, Set: changes(func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("20m") })})
		if err == nil {
			t.Fatalf("a save with pin %q succeeded on a file that does not load", pin)
		}
	}
	if readFile(t, files.project) != broken || s.current() != before {
		t.Fatal("a refused save changed the file or the loaded configuration")
	}
	// An unparsable file reads the same way, naming no content.
	files.write(t, files.topText, "version = 1\nsecret = 'shh\n[")
	got, err = c.ProjectConfig(ctx, project)
	must(t, err)
	if got.Pin != "" || got.Error == nil || strings.Contains(got.Error.Message, "shh") {
		t.Fatalf("snapshot of an unparsable file: %+v", got)
	}

	files.write(t, files.topText, files.projectText)
	got, err = c.ProjectConfig(ctx, project)
	must(t, err)
	if got.Error != nil || got.Pin != good.Pin {
		t.Fatalf("snapshot once the file loads: %+v", got)
	}
	_, err = c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("20m") })})
	must(t, err)
}

// A failure preparing the running service's adoption, or writing the file,
// refuses the save with the reason and leaves the file, its directory and the
// loaded configuration as they were.
func TestProjectConfigSaveRefusesWhenPreparingOrWritingFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, _, files := projectConfigFixture(t)
	got, err := c.ProjectConfig(ctx, project)
	must(t, err)
	edit := ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("20m") })}
	before := s.current()
	leftovers := func() []string {
		entries, err := os.ReadDir(filepath.Dir(files.project))
		must(t, err)
		var names []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				names = append(names, e.Name())
			}
		}
		return names
	}
	unchanged := func(step string) {
		t.Helper()
		if readFile(t, files.project) != files.projectText || s.current() != before || len(leftovers()) != 0 {
			t.Fatalf("%s left the file changed, a temporary file behind, or the loaded configuration changed", step)
		}
	}

	s.setBoundary(func(step string) error {
		if step == "project-config-prepare" {
			return errors.New("engine is not reachable")
		}
		return nil
	})
	_, err = c.SaveProjectConfig(ctx, project, edit)
	if failure := refused(t, err, Internal); !strings.Contains(failure.Message, "engine is not reachable") || !strings.Contains(failure.Message, "unchanged") {
		t.Fatalf("preparation failure: %+v", failure)
	}
	unchanged("a preparation failure")

	s.setBoundary(nil)
	s.setConfigWriter(func(string, []byte) error { return errors.New("disk full") })
	_, err = c.SaveProjectConfig(ctx, project, edit)
	if failure := refused(t, err, Internal); !strings.Contains(failure.Message, "disk full") || !strings.Contains(failure.Message, files.project) {
		t.Fatalf("write failure: %+v", failure)
	}
	unchanged("a write failure")

	s.setBoundary(func(step string) error {
		if step == "project-config-write" {
			return errors.New("interrupted")
		}
		return nil
	})
	s.setConfigWriter(nil)
	_, err = c.SaveProjectConfig(ctx, project, edit)
	refused(t, err, Internal)
	unchanged("an interrupted write")

	// With the injected failures gone the same save goes through.
	s.setBoundary(nil)
	saved, err := c.SaveProjectConfig(ctx, project, edit)
	must(t, err)
	if saved.Outcome != ProjectConfigApplied || s.current().For(project).Project.ChecksTimeout != "20m" {
		t.Fatalf("save after the failures: %+v", saved)
	}
}

// A failure after the file was replaced is the saved-but-not-applied outcome,
// never a refusal: the file holds the saved values at the new pin, the drift
// view shows it as changed, a read flags the saved values as not yet applied,
// and a reload or a later save with the new pin applies them.
func TestProjectConfigSaveReportsAFailureAfterTheReplacement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, how := range []string{"reload", "save"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			s, c, _, files := projectConfigFixture(t)
			got, err := c.ProjectConfig(ctx, project)
			must(t, err)
			s.setBoundary(func(step string) error {
				if step == "project-config-written" {
					return errors.New("cannot reach the scheduler")
				}
				return nil
			})
			saved, err := c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("20m") })})
			must(t, err)
			disk := readFile(t, files.project)
			if saved.Outcome != ProjectConfigSavedNotApplied || !strings.Contains(saved.Reason, "cannot reach the scheduler") || saved.Pin != config.ProjectPin([]byte(disk)) || !strings.Contains(disk, `checks_timeout = "20m"`) {
				t.Fatalf("saved %+v\n%s", saved, disk)
			}
			if timeout := settingOf(t, saved.ProjectConfigResponse, "checks_timeout"); timeout.Value != "20m" || timeout.Effective != "15m" || !timeout.Pending {
				t.Fatalf("saved values read as applied: %+v", timeout)
			}
			if s.current().For(project).Project.ChecksTimeout != "15m" {
				t.Fatal("the failed save was applied")
			}
			view, err := c.Configuration(ctx)
			must(t, err)
			if view.Drift.Files[1].State != ConfigChanged {
				t.Fatalf("drift: %+v", view.Drift)
			}
			read, err := c.ProjectConfig(ctx, project)
			must(t, err)
			if read.Pin != saved.Pin || !settingOf(t, read, "checks_timeout").Pending {
				t.Fatalf("read after: %+v", read)
			}

			s.setBoundary(nil)
			if how == "reload" {
				_, err = c.Reload(ctx)
				must(t, err)
			} else {
				applied, err := c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: saved.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.Landing = str("squash") })})
				must(t, err)
				if applied.Outcome != ProjectConfigApplied {
					t.Fatalf("later save: %+v", applied)
				}
			}
			if p := s.current().For(project).Project; p.ChecksTimeout != "20m" || (how == "save") != (p.Landing == "squash") {
				t.Fatalf("in effect: %+v", p)
			}
			view, err = c.Configuration(ctx)
			must(t, err)
			if view.Drift.Differs {
				t.Fatalf("drift after applying: %+v", view.Drift)
			}
		})
	}
}

// A write that reports failure after the file was in fact replaced, as a
// failing directory sync does, is not reported as a refusal.
func TestProjectConfigSaveReportsAReplacedFileDespiteAWriteError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, c, _, files := projectConfigFixture(t)
	got, err := c.ProjectConfig(ctx, project)
	must(t, err)
	s.setConfigWriter(func(path string, data []byte) error {
		must(t, config.ReplaceFile(path, data))
		return errors.New("directory sync failed")
	})
	saved, err := c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: got.Pin, Set: changes(func(v *ProjectConfigEditValues) { v.ChecksTimeout = str("20m") })})
	must(t, err)
	if saved.Outcome != ProjectConfigSavedNotApplied || !strings.Contains(saved.Reason, "directory sync failed") || !strings.Contains(readFile(t, files.project), `"20m"`) {
		t.Fatalf("saved %+v", saved)
	}
}

// The endpoints follow the web listener's rules: a JSON content type for a
// save, and a Host naming the listener.
func TestProjectConfigEndpointsFollowTheWebListenerRules(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	withListen(t, opts, "web = \"127.0.0.1:0\"\n")
	s, c := start(t, opts)
	got, err := c.ProjectConfig(context.Background(), project)
	must(t, err)
	url := "http://" + s.WebAddr() + Prefix + "/projects/config/" + string(project)
	body := `{"pin":"` + got.Pin + `","set":{"landing":"squash"}}`
	if code, out := exchange(t, webHTTP(), "GET", url, "", "", ""); code != 200 || !strings.Contains(string(out), got.Pin) {
		t.Fatalf("read: %d %s", code, out)
	}
	if code, _ := exchange(t, webHTTP(), "GET", url, "", "", "evil.example"); code != 403 {
		t.Fatalf("read with a foreign Host: %d", code)
	}
	for _, contentType := range []string{"", "text/plain"} {
		if code, _ := exchange(t, webHTTP(), "PUT", url, contentType, body, ""); code != 403 {
			t.Fatalf("save with content type %q: %d", contentType, code)
		}
	}
	if code, _ := exchange(t, webHTTP(), "PUT", url, "application/json", body, "evil.example"); code != 403 {
		t.Fatalf("save with a foreign Host: %d", code)
	}
	if code, out := exchange(t, webHTTP(), "PUT", url, "application/json", body, ""); code != 200 || !strings.Contains(string(out), `"outcome":"applied"`) {
		t.Fatalf("save: %d %s", code, out)
	}
	if code, _ := exchange(t, webHTTP(), "DELETE", url, "application/json", "", ""); code != 501 {
		t.Fatalf("unsupported method: %d", code)
	}
}

// changes builds the values of a save.
func changes(set func(*ProjectConfigEditValues)) ProjectConfigEditValues {
	var v ProjectConfigEditValues
	set(&v)
	return v
}
