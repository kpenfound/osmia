package verification

import (
	"path"
	"strings"

	"github.com/kpenfound/osmia/internal/callargs"
)

// DefaultMapDir is the feature-map directory of a project whose setting does
// not name one.
const DefaultMapDir = "docs/verification"

// Setting is a project's optional verification setting. The zero value is a
// project without the setting: verification is disabled.
type Setting struct {
	// Call holds the base `dagger call` arguments that select the lever.
	Call []string `toml:"call" json:"call,omitempty"`
	// MapDir is the repository-relative feature-map directory.
	MapDir string `toml:"map_dir" json:"map_dir,omitempty"`
	// LeverPaths are repository path globs of the lever's source, used to
	// flag changes to it.
	LeverPaths []string `toml:"lever_paths" json:"lever_paths,omitempty"`
}

// Enabled reports whether the project has the verification setting.
func (s Setting) Enabled() bool { return len(s.Call) > 0 }

// Directory is the feature-map directory, DefaultMapDir when the setting
// names none.
func (s Setting) Directory() string {
	if s.MapDir == "" {
		return DefaultMapDir
	}
	return s.MapDir
}

// Validate checks the setting and returns it with defaults applied. A zero
// setting is valid and stays disabled. On failure field names the offending
// key and reason says what is wrong, without repeating its value.
func (s Setting) Validate() (out Setting, field, reason string) {
	if s.Call == nil && s.MapDir == "" && s.LeverPaths == nil {
		return Setting{}, "", ""
	}
	if len(s.Call) == 0 {
		return s, "call", "must name the base dagger call arguments that select the lever"
	}
	if err := callargs.Validate("verification call", s.Call); err != nil {
		return s, "call", err.Error()
	}
	if callargs.SetsRunID(s.Call) {
		return s, "call", "must not set " + callargs.RunIDFlag + ": the service gives every run its own"
	}
	s.Call = append([]string(nil), s.Call...)
	if s.MapDir != "" {
		if reason := checkRepositoryPath(s.MapDir, false); reason != "" {
			return s, "map_dir", reason
		}
	}
	s.MapDir = s.Directory()
	for _, glob := range s.LeverPaths {
		if reason := checkRepositoryPath(glob, true); reason != "" {
			return s, "lever_paths", reason
		}
	}
	s.LeverPaths = append([]string(nil), s.LeverPaths...)
	return s, "", ""
}

func checkRepositoryPath(p string, glob bool) string {
	if p == "" || strings.ContainsAny(p, "\x00\r\n\\") {
		return "must be a non-empty repository-relative path on one line"
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "~") {
		return "must be relative to the repository"
	}
	if path.Clean(p) != p || p == "." {
		return "must be a clean path without empty, . or .. segments"
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return "must stay inside the repository"
		}
	}
	if glob {
		if err := CheckGlob(p); err != nil {
			return "is not a valid path glob"
		}
	} else if strings.ContainsAny(p, "*?[") {
		return "must be a directory, not a glob"
	}
	return ""
}
