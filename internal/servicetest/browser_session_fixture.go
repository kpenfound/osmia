package servicetest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SessionWebFixture starts a service with a web listener and a registered
// Project with a real local upstream remote, the production enforcement
// wiring bound to a fake engine, so a browser test can open and drive a
// session through the real sandbox, thread and Git machinery.
func SessionWebFixture(t *testing.T) (*service.Service, *service.Client, config.ProjectID, *fixturekit.SessionEngine, string) {
	t.Helper()
	opts, clone := ProjectFixture(t)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString("[listen]\nweb = \"127.0.0.1:0\"\n")
	fixturekit.Must(t, err)
	fixturekit.Must(t, configFile.Close())
	home := filepath.Dir(clone)
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", clone, "add", "README.md")
	fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	fixturekit.DemoGit(t, home, "-C", clone, "branch", "-M", "main")
	bare := filepath.Join(home, "remotes", "dagger", "dagger.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(bare), 0700))
	fixturekit.DemoGit(t, home, "clone", "--quiet", "--bare", "--no-hardlinks", clone, bare)
	fixturekit.DemoGit(t, home, "-C", clone, "remote", "add", "upstream", bare)
	engine := &fixturekit.SessionEngine{Sessions: &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}}
	hosts := &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: engine.Sessions}}
	enforced := service.EnforceThreadsForTest(opts, service.Enforcement{Engine: engine, Hosts: hosts})
	opts = enforced
	s, c := Start(t, opts)
	added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	WriteCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	return s, c, added.Project.ID, engine, clone
}
