package fixturekit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
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

// EditProjectSettings applies edit to the text of project id's settings: the
// stored revision a restart loads, once the root's import is complete, and the
// project's file. Each gets its own text, since the file may carry a retired
// mark the stored revision does not. A fixture changes a project's settings
// between two runs of the service this way.
func EditProjectSettings(t *testing.T, rootDirectory string, id config.ProjectID, edit func(text string) string) {
	t.Helper()
	root, err := config.ResolveRoot(rootDirectory, "")
	Must(t, err)
	store := projectstore.Open(root)
	if done, err := store.Imported(); err != nil {
		t.Fatal(err)
	} else if done {
		current, err := store.Current(id)
		Must(t, err)
		_, err = store.Replace(id, current.Pin, []byte(edit(string(current.Data))))
		Must(t, err)
	}
	path, err := root.ProjectConfig(id)
	Must(t, err)
	if data, err := os.ReadFile(path); err == nil {
		Must(t, os.WriteFile(path, []byte(edit(string(data))), 0600))
	}
}
