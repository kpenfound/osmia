package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The configuration view compares the top-level file on disk with the loaded
// configuration, and nothing else: an edit changes it, one that does not
// validate or cannot be read makes it invalid, and one that changes nothing
// loaded, such as a comment, leaves it unchanged. A retired project file, a
// project's stored settings and the list of active_projects a leftover
// top-level file carries are never compared. Reading the view applies nothing
// and writes nothing.
func TestConfigReportsDriftOfTheTopLevelFileOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := configFiles(t, opts)
	s, c := start(t, opts)
	before, err := c.Configuration(ctx)
	must(t, err)
	top := filepath.Join(before.Root, "config.toml")
	other := "p_00000000000000000000000000000009"
	relisted := strings.Replace(files.topText, `active_projects = ["`+string(project)+`"]`, `active_projects = ["`+other+`"]`, 1)

	for _, step := range []struct {
		name, top string
		want      ConfigFile
		differs   bool
	}{
		{"as loaded", files.topText, ConfigFile{Path: top, State: ConfigUnchanged}, false},
		{"a comment", files.topText + "# a note\n", ConfigFile{Path: top, State: ConfigUnchanged}, false},
		{"a leftover active_projects naming another project", relisted, ConfigFile{Path: top, State: ConfigUnchanged}, false},
		{"top-level edit", files.topText + "[capacity]\nmasons = 7\n", ConfigFile{Path: top, State: ConfigChanged}, true},
		{"restart-only edit", files.topText + "[listen]\nweb = \"127.0.0.1:8484\"\n", ConfigFile{Path: top, State: ConfigChanged}, true},
		{"invalid top level", files.topText + "[capacity]\nmasons = 0\n", ConfigFile{Path: top, State: ConfigInvalid, Reason: "capacity.masons: must be positive"}, true},
	} {
		// The retired project file differs from what was loaded on every step,
		// and is invalid on some: it is never read.
		must(t, os.WriteFile(files.top, []byte(step.top), 0600))
		must(t, os.WriteFile(files.project, []byte("landing = \"sideways\"\n"), 0600))
		got, err := c.Configuration(ctx)
		must(t, err)
		want := ConfigDrift{Differs: step.differs, Files: []ConfigFile{step.want}}
		if !reflect.DeepEqual(got.Drift, want) {
			t.Fatalf("%s: drift\n%+v\nwant\n%+v", step.name, got.Drift, want)
		}
		if got.Digest != before.Digest || !reflect.DeepEqual(got.Effective, before.Effective) || got.LastError != nil {
			t.Fatalf("%s: reading the configuration applied it: %+v", step.name, got)
		}
		if data, err := os.ReadFile(files.top); err != nil || string(data) != step.top {
			t.Fatalf("%s: reading the configuration rewrote the top-level file", step.name)
		}
	}
	if _, err := os.Stat(filepath.Join(before.Root, "projects", other)); !os.IsNotExist(err) {
		t.Fatalf("reading the configuration created another project's directory: %v", err)
	}

	// A top-level file that cannot be read is invalid, and a retired project
	// file that cannot be read does not matter.
	must(t, os.WriteFile(files.top, []byte(files.topText), 0600))
	must(t, os.Remove(files.project))
	must(t, os.Mkdir(files.project, 0700))
	got, err := c.Configuration(ctx)
	must(t, err)
	if got.Drift.Differs || got.Drift.Files[0].State != ConfigUnchanged {
		t.Fatalf("unreadable retired project file: %+v", got.Drift)
	}
	must(t, os.Remove(files.project))

	// Once the top-level file is reloaded, nothing differs.
	must(t, os.WriteFile(files.top, []byte(files.topText+"[capacity]\nmasons = 7\n"), 0600))
	_, err = c.Reload(ctx)
	must(t, err)
	got, err = c.Configuration(ctx)
	must(t, err)
	if got.Drift.Differs || got.Digest == before.Digest || s.current().Capacity.Masons != 7 {
		t.Fatalf("after a reload: %+v", got)
	}
	if !reflect.DeepEqual(got.Effective.ActiveProjects, []string{string(project)}) || len(got.Effective.Projects) != 1 {
		t.Fatalf("the reload changed the active projects: %v", got.Effective.ActiveProjects)
	}
}
