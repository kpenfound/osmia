package fixturekit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
)

// ReloadFiles holds the fixture's top-level configuration path and its
// original contents.
type ReloadFiles struct {
	Top     string
	TopText string
}

// ConfigFiles reads the top-level configuration of a fixture.
func ConfigFiles(t *testing.T, root string) ReloadFiles {
	t.Helper()
	f := ReloadFiles{Top: filepath.Join(root, "config.toml")}
	top, err := os.ReadFile(f.Top)
	Must(t, err)
	f.TopText = string(top)
	return f
}

// Write replaces the top-level configuration file.
func (f ReloadFiles) Write(t *testing.T, top string) {
	t.Helper()
	Must(t, os.WriteFile(f.Top, []byte(top), 0600))
}

// EditProjectSettings applies edit to the text of project id's current stored
// revision and records the result as the next revision. A fixture changes a
// project's settings between two runs of the service this way.
func EditProjectSettings(t *testing.T, rootDirectory string, id config.ProjectID, edit func(text string) string) {
	t.Helper()
	root, err := config.ResolveRoot(rootDirectory, "")
	Must(t, err)
	store := projectstore.Open(root)
	current, err := store.Current(id)
	Must(t, err)
	_, err = store.Replace(id, current.Pin, []byte(edit(string(current.Data))))
	Must(t, err)
}

// SaveProjectSettings stores p's settings as the next revision of its stored
// settings, or as the first when the project is not registered yet.
func SaveProjectSettings(t *testing.T, rootDirectory string, p config.Project) {
	t.Helper()
	data, err := config.ProjectConfigBytes(p)
	Must(t, err)
	root, err := config.ResolveRoot(rootDirectory, "")
	Must(t, err)
	if current, err := projectstore.Open(root).Current(p.ID); err == nil {
		_, err = projectstore.Open(root).Replace(p.ID, current.Pin, data)
		Must(t, err)
		return
	}
	RegisterProject(t, rootDirectory, p.ID, string(data))
}
