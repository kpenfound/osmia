package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The configuration view compares each configuration file on disk with the
// loaded configuration: an edit changes its file, one that does not validate
// or cannot be read makes it invalid, and one that changes nothing loaded,
// such as a comment, leaves it unchanged. Reading the view applies nothing
// and writes nothing.
func TestConfigReportsDriftOfEveryFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := configFiles(t, opts)
	s, c := start(t, opts)
	before, err := c.Configuration(ctx)
	must(t, err)
	top := filepath.Join(before.Root, "config.toml")
	proj := filepath.Join(before.Root, "projects", string(project), "config.toml")
	other := "p_00000000000000000000000000000009"

	for _, step := range []struct {
		name, top, project string
		want               []ConfigFile
	}{
		{"as loaded", files.topText, files.projectText, []ConfigFile{{Path: top, State: ConfigUnchanged}, {Path: proj, Project: project, State: ConfigUnchanged}}},
		{"a comment", files.topText + "# a note\n", files.projectText + "# a note\n", []ConfigFile{{Path: top, State: ConfigUnchanged}, {Path: proj, Project: project, State: ConfigUnchanged}}},
		{"top-level edit", files.topText + "[capacity]\nmasons = 7\n", files.projectText, []ConfigFile{{Path: top, State: ConfigChanged}, {Path: proj, Project: project, State: ConfigUnchanged}}},
		{"restart-only edit", files.topText + "[listen]\nweb = \"127.0.0.1:8484\"\n", files.projectText, []ConfigFile{{Path: top, State: ConfigChanged}, {Path: proj, Project: project, State: ConfigUnchanged}}},
		{"project edit", files.topText, files.projectText + "landing = \"squash\"\n", []ConfigFile{{Path: top, State: ConfigUnchanged}, {Path: proj, Project: project, State: ConfigChanged}}},
		{"invalid project", files.topText + "[capacity]\nmasons = 7\n", files.projectText + "landing = \"sideways\"\n", []ConfigFile{{Path: top, State: ConfigChanged}, {Path: proj, Project: project, State: ConfigInvalid, Reason: "landing: expected commit-per-unit or squash"}}},
		{"invalid top level", files.topText + "[capacity]\nmasons = 0\n", files.projectText + "landing = \"squash\"\n", []ConfigFile{{Path: top, State: ConfigInvalid, Reason: "capacity.masons: must be positive"}, {Path: proj, Project: project, State: ConfigChanged}}},
		{"another active project", "version = 1\nactive_projects = [\"" + other + "\"]\n" + files.topText[len("version = 1\nactive_projects = [\""+string(project)+"\"]\n"):], files.projectText,
			[]ConfigFile{{Path: top, State: ConfigChanged}, {Path: proj, Project: project, State: ConfigUnchanged}, {Path: filepath.Join(before.Root, "projects", other, "config.toml"), State: ConfigInvalid, Reason: "cannot be read"}}},
	} {
		files.write(t, step.top, step.project)
		got, err := c.Configuration(ctx)
		must(t, err)
		want := ConfigDrift{Differs: step.name != "as loaded" && step.name != "a comment", Files: step.want}
		if !reflect.DeepEqual(got.Drift, want) {
			t.Fatalf("%s: drift\n%+v\nwant\n%+v", step.name, got.Drift, want)
		}
		if got.Digest != before.Digest || !reflect.DeepEqual(got.Effective, before.Effective) || got.LastError != nil {
			t.Fatalf("%s: reading the configuration applied it: %+v", step.name, got)
		}
		for path, text := range map[string]string{files.top: step.top, files.project: step.project} {
			data, err := os.ReadFile(path)
			must(t, err)
			if string(data) != text {
				t.Fatalf("%s: reading the configuration rewrote %s", step.name, path)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(before.Root, "projects", other)); !os.IsNotExist(err) {
		t.Fatalf("reading the configuration created another project's directory: %v", err)
	}

	// A file that cannot be read is invalid.
	must(t, os.WriteFile(files.top, []byte(files.topText), 0600))
	must(t, os.Remove(files.project))
	must(t, os.Mkdir(files.project, 0700))
	got, err := c.Configuration(ctx)
	must(t, err)
	if want := (ConfigFile{Path: proj, Project: project, State: ConfigInvalid, Reason: "cannot be read"}); !got.Drift.Differs || got.Drift.Files[0].State != ConfigUnchanged || got.Drift.Files[1] != want {
		t.Fatalf("unreadable project file: %+v", got.Drift)
	}
	must(t, os.Remove(files.project))

	// Once the files on disk are reloaded, nothing differs.
	files.write(t, files.topText+"[capacity]\nmasons = 7\n", files.projectText)
	_, err = c.Reload(ctx)
	must(t, err)
	got, err = c.Configuration(ctx)
	must(t, err)
	if got.Drift.Differs || got.Digest == before.Digest || s.current().Capacity.Masons != 7 {
		t.Fatalf("after a reload: %+v", got)
	}
}
