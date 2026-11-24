package fixturekit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
)

const topLevelFixture = `version = 1
workspaces = "git"
[profiles.default]
agent = "claude"
model = "test"
[profiles.other]
agent = "codex"
model = "other"
`

// ConfigAt writes a minimal top-level configuration under home and registers
// Project in the root's project store. Its workstreams are created on Git
// worktrees whether or not jj is installed.
func ConfigAt(t *testing.T, home string) config.Options {
	t.Helper()
	return ConfigWithSettings(t, home, `version = 1
upstream = "upstream/repo"
fork = "owner/repo"
clone = "`+filepath.Join(home, "clone")+`"
`)
}

// ConfigWithSettings is ConfigAt with settings as Project's first revision.
func ConfigWithSettings(t *testing.T, home, settings string) config.Options {
	t.Helper()
	root := filepath.Join(home, "root")
	Must(t, os.MkdirAll(root, 0700))
	Must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(topLevelFixture), 0600))
	RegisterProject(t, root, Project, settings)
	return config.Options{Root: root}
}

// RegisterProject adds id to the project store of the root at rootDirectory
// with settings as its first revision.
func RegisterProject(t *testing.T, rootDirectory string, id config.ProjectID, settings string) {
	t.Helper()
	root, err := config.ResolveRoot(rootDirectory, "")
	Must(t, err)
	store := projectstore.Open(root)
	_, err = store.WriteFirst(id, []byte(settings))
	Must(t, err)
	Must(t, store.Add(id))
}
