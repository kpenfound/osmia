package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Load failures are field errors naming the file and field, and a file the
// TOML decoder rejects is named by key and line without the decoder's text.
func TestLoadFailuresNameTheFileAndField(t *testing.T) {
	for _, tc := range []struct {
		name, top, project, file, field, reason string
	}{
		{"validation", topConfig + "[capacity]\nmasons = 0\n", projectConfig, "config.toml", "capacity.masons", "must be positive"},
		{"project validation", topConfig, projectConfig + "landing = 'merge'\n", "projects/" + pid + "/config.toml", "landing", "expected commit-per-unit or squash"},
		{"wrong type", topConfig + "[capacity]\nmasons = 'secret-value'\n", projectConfig, "config.toml", "capacity.masons", "value has the wrong type at line 7"},
		{"project wrong type", topConfig, projectConfig + "upstream_rebase = 0\n", "projects/" + pid + "/config.toml", "upstream_rebase", "value has the wrong type at line 5"},
		{"syntax", topConfig + "token = 'secret-value'\n[broken", projectConfig, "config.toml", "profiles.default", "invalid TOML at line 6"},
		{"unknown key", topConfig + "secret = 'secret-value'\n", projectConfig, "config.toml", "profiles.default.secret", "unknown configuration key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := fixture(t, tc.top, tc.project)
			_, err := Load(opts)
			root, _ := ResolveRoot("", opts.Home)
			want := &FieldError{Path: filepath.Join(root.String(), tc.file), Field: tc.field, Reason: tc.reason}
			var got *FieldError
			if !errors.As(err, &got) || got.Path != want.Path || got.Field != want.Field || got.Reason != want.Reason {
				t.Fatalf("got %#v (%v), want %#v", got, err, want)
			}
			if err.Error() != want.Error() || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error text: %q", err)
			}
		})
	}
	opts := fixture(t, topConfig, projectConfig)
	root, _ := ResolveRoot("", opts.Home)
	path := filepath.Join(root.String(), "projects", pid, "config.toml")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	var got *FieldError
	if _, err := Load(opts); !errors.As(err, &got) || err.Error() != path+": cannot be read" || !os.IsNotExist(errors.Unwrap(err)) {
		t.Fatalf("unreadable project file: %v", err)
	}
}
