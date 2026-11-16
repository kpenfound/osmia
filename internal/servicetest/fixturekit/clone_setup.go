package fixturekit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CloneSetup is a project root, a clone with tracked and untracked files and
// the fake engine a service under test runs its role turns in.
type CloneSetup struct {
	Config   config.Options
	Clone    string
	Engine   *DemoEngine
	Sessions *DemoSessions
	Clock    *DemoClock
}

// NewCloneSetup commits internal/trace/git.go, CODEOWNERS and AGENTS.md to the
// clone, and leaves an untracked scratch directory, a secret file and an
// ignored directory beside them, so a seed shows which files it read.
func NewCloneSetup(t *testing.T) CloneSetup {
	t.Helper()
	cfg, clone := ProjectConfig(t)
	home := filepath.Dir(clone)
	Must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	Must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	Must(t, os.WriteFile(filepath.Join(clone, "CODEOWNERS"), []byte("/internal/ @core\n"), 0600))
	Must(t, os.WriteFile(filepath.Join(clone, "AGENTS.md"), []byte("# Agents\n"), 0600))
	Must(t, os.WriteFile(filepath.Join(clone, "secret.env"), []byte("TOKEN="+DemoSecret+"\n"), 0600))
	Must(t, os.MkdirAll(filepath.Join(clone, "scratch"), 0700))
	Must(t, os.WriteFile(filepath.Join(clone, "scratch", "notes.txt"), []byte("untracked\n"), 0600))
	Must(t, os.MkdirAll(filepath.Join(clone, "node_modules", "left-pad"), 0700))
	Must(t, os.WriteFile(filepath.Join(clone, "node_modules", "left-pad", "index.js"), []byte("ignored\n"), 0600))
	Must(t, os.WriteFile(filepath.Join(clone, ".git", "info", "exclude"), []byte("node_modules/\n"), 0600))
	DemoGit(t, home, "-C", clone, "add", "internal", "CODEOWNERS", "AGENTS.md")
	DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	sessions := &DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &DemoEngine{Sessions: sessions, Turns: map[string]DemoTurn{}}
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	clock := &DemoClock{At: DemoStart}
	return CloneSetup{Config: cfg, Clone: clone, Engine: engine, Sessions: sessions, Clock: clock}
}
