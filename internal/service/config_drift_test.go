package service

import (
	"context"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigReportsDriftOfTheTopLevelFileOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := configFiles(t, opts)
	s, c := start(t, opts)
	before, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	top := filepath.Join(before.Root, "config.toml")

	for _, step := range []struct {
		name, top string
		want      ConfigFile
		differs   bool
	}{
		{"as loaded", files.topText, ConfigFile{Path: top, State: ConfigUnchanged}, false},
		{"a comment", files.topText + "# a note\n", ConfigFile{Path: top, State: ConfigUnchanged}, false},
		{"top-level edit", files.topText + "[capacity]\nmasons = 7\n", ConfigFile{Path: top, State: ConfigChanged}, true},
		{"restart-only edit", files.topText + "[listen]\nweb = \"127.0.0.1:8484\"\n", ConfigFile{Path: top, State: ConfigChanged}, true},
		{"invalid top level", files.topText + "[capacity]\nmasons = 0\n", ConfigFile{Path: top, State: ConfigInvalid, Reason: "capacity.masons: must be positive"}, true},
	} {
		fixturekit.Must(t, os.WriteFile(files.top, []byte(step.top), 0600))
		got, err := c.Configuration(ctx)
		fixturekit.Must(t, err)
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
	fixturekit.Must(t, os.WriteFile(files.top, []byte(files.topText+"[capacity]\nmasons = 7\n"), 0600))
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	got, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if got.Drift.Differs || got.Digest == before.Digest || s.current().Capacity.Masons != 7 {
		t.Fatalf("after a reload: %+v", got)
	}
	if !reflect.DeepEqual(got.Effective.ActiveProjects, []string{string(fixturekit.Project)}) || len(got.Effective.Projects) != 1 {
		t.Fatalf("the reload changed the active projects: %v", got.Effective.ActiveProjects)
	}
}
