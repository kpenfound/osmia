package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectCreationIsOffUntilBothKeysAreSet(t *testing.T) {
	for name, table := range map[string]string{
		"no table":   "",
		"empty":      "[projects]\n",
		"only owner": "[projects]\nrepo_owner = 'octo'\n",
		"only dir":   "[projects]\nclone_dir = '~/new'\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(fixture(t, topConfig+table, projectConfig))
			if err != nil || c.ProjectCreation.CreationEnabled() {
				t.Fatalf("creation enabled without both keys: %+v %v", c.ProjectCreation, err)
			}
			want := "projects.repo_owner"
			if name == "only owner" {
				want = "projects.clone_dir"
			}
			if got := c.ProjectCreation.MissingSetting(); got != want {
				t.Fatalf("missing setting %q, want %q", got, want)
			}
		})
	}
}

func TestProjectCreationExpandsTheCloneDirectory(t *testing.T) {
	opts := fixture(t, topConfig+"[projects]\nrepo_owner = 'octo-cat'\nclone_dir = '~/new/work'\n", projectConfig)
	c, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	home, err := filepath.EvalSymlinks(opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "new", "work")
	if !c.ProjectCreation.CreationEnabled() || c.ProjectCreation.RepoOwner != "octo-cat" || c.ProjectCreation.MissingSetting() != "" {
		t.Fatalf("creation not enabled: %+v", c.ProjectCreation)
	}
	if c.ProjectCreation.CloneDir != want {
		t.Fatalf("clone_dir %q, want %q", c.ProjectCreation.CloneDir, want)
	}
	absolute := filepath.Join(opts.Home, "elsewhere")
	c, err = Load(fixture(t, topConfig+"[projects]\nrepo_owner = 'octo'\nclone_dir = '"+absolute+"'\n", projectConfig))
	if err != nil || !c.ProjectCreation.CreationEnabled() || filepath.Base(c.ProjectCreation.CloneDir) != "elsewhere" {
		t.Fatalf("absolute clone_dir: %+v %v", c.ProjectCreation, err)
	}
}

func TestProjectCreationRefusesInvalidSettingsByField(t *testing.T) {
	for name, tc := range map[string]struct{ table, field string }{
		"unknown key":      {"[projects]\nclone_protocol = 'ssh'\n", "projects.clone_protocol"},
		"owner with slash": {"[projects]\nrepo_owner = 'octo/cat'\n", "projects.repo_owner"},
		"owner url":        {"[projects]\nrepo_owner = 'https://github.com/octo'\n", "projects.repo_owner"},
		"owner hyphen":     {"[projects]\nrepo_owner = '-octo'\n", "projects.repo_owner"},
		"owner doubled":    {"[projects]\nrepo_owner = 'oc--to'\n", "projects.repo_owner"},
		"owner too long":   {"[projects]\nrepo_owner = '" + strings.Repeat("a", 40) + "'\n", "projects.repo_owner"},
		"owner type":       {"[projects]\nrepo_owner = 7\n", "projects.repo_owner"},
		"relative dir":     {"[projects]\nrepo_owner = 'octo'\nclone_dir = 'new'\n", "projects.clone_dir"},
		"other user home":  {"[projects]\nrepo_owner = 'octo'\nclone_dir = '~root/new'\n", "projects.clone_dir"},
		"dir with newline": {"[projects]\nrepo_owner = 'octo'\nclone_dir = \"/tmp/a\\nb\"\n", "projects.clone_dir"},
		"dir is the root":  {"[projects]\nrepo_owner = 'octo'\nclone_dir = '~/.local/share/osmia'\n", "projects.clone_dir"},
		"dir in the root":  {"[projects]\nrepo_owner = 'octo'\nclone_dir = '~/.local/share/osmia/clones'\n", "projects.clone_dir"},
		"dir above root":   {"[projects]\nrepo_owner = 'octo'\nclone_dir = '~/.local'\n", "projects.clone_dir"},
		"key type":         {"[projects]\nrepo_owner = 'octo'\nclone_dir = 4\n", "projects.clone_dir"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(fixture(t, topConfig+tc.table, projectConfig))
			var field *FieldError
			if !errors.As(err, &field) || field.Field != tc.field {
				t.Fatalf("got %v, want a field error naming %s", err, tc.field)
			}
		})
	}
}
