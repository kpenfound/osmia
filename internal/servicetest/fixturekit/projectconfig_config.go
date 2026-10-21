package fixturekit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

// ProjectConfig writes a root without a Project and a local Git clone.
func ProjectConfig(t *testing.T) (config.Options, string) {
	t.Helper()
	home, err := os.MkdirTemp("", "op-")
	Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	resolved, err := config.ResolveRoot(filepath.Join(home, "root"), "")
	Must(t, err)
	root := resolved.String()
	Must(t, os.MkdirAll(root, 0700))
	Must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(CommentedConfig), 0600))
	clone := filepath.Join(home, "clone")
	Must(t, os.Mkdir(clone, 0700))
	DemoGit(t, home, "init", "--quiet", clone)
	return config.Options{Root: root}, clone
}
