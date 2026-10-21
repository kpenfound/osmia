package fixturekit

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

// fixtureAt writes a minimal root and Project configuration under home. Its
// workstreams are created on Git worktrees whether or not jj is installed.
func ConfigAt(t *testing.T, home string) config.Options {
	t.Helper()
	root := filepath.Join(home, "root")
	Must(t, os.MkdirAll(filepath.Join(root, "projects", string(Project)), 0700))
	Must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(fmt.Sprintf(`version = 1
active_projects = [%q]
workspaces = "git"
[profiles.default]
agent = "claude"
model = "test"
[profiles.other]
agent = "codex"
model = "other"
`, Project)), 0600))
	Must(t, os.WriteFile(filepath.Join(root, "projects", string(Project), "config.toml"), []byte(`version = 1
upstream = "upstream/repo"
fork = "owner/repo"
clone = "`+filepath.Join(home, "clone")+`"
`), 0600))
	return config.Options{Root: root}
}
