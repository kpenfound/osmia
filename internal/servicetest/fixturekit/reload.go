package fixturekit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

// ReloadFiles holds fixture configuration paths and their original contents.
type ReloadFiles struct {
	Top, Project         string
	TopText, ProjectText string
}

// ConfigFiles reads the top-level and project configuration of a fixture.
func ConfigFiles(t *testing.T, root string, project config.ProjectID) ReloadFiles {
	t.Helper()
	f := ReloadFiles{Top: filepath.Join(root, "config.toml"), Project: filepath.Join(root, "projects", string(project), "config.toml")}
	top, err := os.ReadFile(f.Top)
	Must(t, err)
	p, err := os.ReadFile(f.Project)
	Must(t, err)
	f.TopText, f.ProjectText = string(top), string(p)
	return f
}

// Write replaces both fixture configuration files.
func (f ReloadFiles) Write(t *testing.T, top, project string) {
	t.Helper()
	Must(t, os.WriteFile(f.Top, []byte(top), 0600))
	Must(t, os.WriteFile(f.Project, []byte(project), 0600))
}
