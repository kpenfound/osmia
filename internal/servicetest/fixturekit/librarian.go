package fixturekit

import (
	"context"
	"errors"
	"fmt"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"path/filepath"
	"testing"
)

type LibrarianSetup struct {
	Config   config.Options
	Clone    string
	Engine   *DemoEngine
	Sessions *DemoSessions
	Clock    *DemoClock
}

// LibrarianContainer runs the librarian in a container, the only sandbox
// the core executor accepts.
const LibrarianContainer = `[roles.librarian]
sandbox = "container"
image = "fixture-image"
`

func NewLibrarianSetup(t *testing.T) LibrarianSetup {
	t.Helper()
	cfg, clone := ProjectConfig(t)
	home := filepath.Dir(clone)
	configFile, err := os.OpenFile(filepath.Join(cfg.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	Must(t, err)
	_, err = configFile.WriteString(LibrarianContainer)
	Must(t, errors.Join(err, configFile.Close()))
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
	return LibrarianSetup{Config: cfg, Clone: clone, Engine: engine, Sessions: sessions, Clock: clock}
}

// script installs the fake librarian's behaviour for one turn: it writes the
// given output files and returns a successful result.
func ScriptLibrarian(engine *DemoEngine, turn string, files map[string]string, check func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) error) {
	engine.Mu.Lock()
	defer engine.Mu.Unlock()
	engine.Turns[turn] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		var err error
		if check != nil {
			err = check(ctx, req, verified, tools)
		}
		for path, content := range files {
			if _, e := CallTool(ctx, tools, "file_write", map[string]any{"path": path, "content": content}); e != nil {
				err = errors.Join(err, fmt.Errorf("write %s: %w", path, e))
			}
		}
		if err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + turn, ResultText: "Knowledge base written", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
}
