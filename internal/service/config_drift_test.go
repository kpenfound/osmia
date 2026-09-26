package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
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
	otherProfile := "[profiles.other]\nagent = \"codex\"\nmodel = \"other\"\n"
	if !strings.Contains(files.topText, otherProfile) {
		t.Fatalf("the fixture has no other profile:\n%s", files.topText)
	}
	activating := func(id string) string {
		return "version = 1\nactive_projects = [\"" + id + "\"]\n" + files.topText[len("version = 1\nactive_projects = [\""+string(project)+"\"]\n"):]
	}

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
		// Each file passes on its own, but the project's classifier names a
		// profile the edited top level no longer has, so a reload fails on
		// the project file.
		{"valid alone, invalid together", strings.Replace(files.topText, otherProfile, "", 1), files.projectText + "classifier = \"other\"\n",
			[]ConfigFile{{Path: top, State: ConfigChanged}, {Path: proj, Project: project, State: ConfigInvalid, Reason: "classifier: unknown profile other"}}},
		{"another active project", activating(other), files.projectText,
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

	// A reload failure that names no file, here another active project's
	// directory resolving outside the root, marks the top-level file invalid.
	alias := "p_00000000000000000000000000000008"
	must(t, os.Symlink(t.TempDir(), filepath.Join(before.Root, "projects", alias)))
	files.write(t, activating(alias), files.projectText)
	got, err := c.Configuration(ctx)
	must(t, err)
	if want := (ConfigDrift{Differs: true, Files: []ConfigFile{{Path: top, State: ConfigInvalid, Reason: reloadError(errors.New("unnamed"), time.Time{}).Message}, {Path: proj, Project: project, State: ConfigUnchanged}}}); !reflect.DeepEqual(got.Drift, want) {
		t.Fatalf("a reload failure naming no file: drift\n%+v\nwant\n%+v", got.Drift, want)
	}
	if got.Digest != before.Digest || got.LastError != nil {
		t.Fatalf("a reload failure naming no file applied it: %+v", got)
	}
	must(t, os.Remove(filepath.Join(before.Root, "projects", alias)))

	// A file that cannot be read is invalid.
	must(t, os.WriteFile(files.top, []byte(files.topText), 0600))
	must(t, os.Remove(files.project))
	must(t, os.Mkdir(files.project, 0700))
	got, err = c.Configuration(ctx)
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

// A top-level file without active_projects lists the same projects as the
// empty list project remove leaves loaded.
func TestTopLevelDigestTreatsNoActiveProjectsAsEmpty(t *testing.T) {
	t.Parallel()
	loaded := &config.Config{Version: 1, ActiveProjects: []string{}}
	if topLevelDigest(loaded) != topLevelDigest(&config.Config{Version: 1}) {
		t.Fatal("an absent active_projects differs from an empty one")
	}
	if topLevelDigest(loaded) == topLevelDigest(&config.Config{Version: 1, ActiveProjects: []string{string(project)}}) {
		t.Fatal("a listed project does not differ")
	}
}
