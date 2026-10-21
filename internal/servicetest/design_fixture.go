package servicetest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewDesignEnforcementFixture starts a service with the real production
// enforcement (internal/service/enforcement.go), a fake execution engine
// and a fake in-memory MCP transport, on an onboarded Project with a
// committed clone, so a design session's turn exercises the same grant,
// sandbox and tool-registry machinery a production service would. The
// chief-of-staff role runs in a container, the only sandbox mode every
// platform's confiner can enforce (compare chiefDemoRoles): the host
// sandbox every role defaults to needs Landlock or its Seatbelt equivalent
// to confine a session at all, which not every kernel has.
func NewDesignEnforcementFixture(t *testing.T, web ...bool) (*service.Service, *service.Client, config.ProjectID, *fixturekit.DemoEngine) {
	t.Helper()
	opts, clone := ProjectFixture(t)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	if len(web) > 0 && web[0] {
		_, err = configFile.WriteString("[listen]\nweb = \"127.0.0.1:0\"\n")
		fixturekit.Must(t, err)
	}
	_, err = configFile.WriteString("[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n")
	fixturekit.Must(t, errors.Join(err, configFile.Close()))
	home := filepath.Dir(clone)
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("hi\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", clone, "add", "README.md")
	fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	sessions := &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &fixturekit.DemoEngine{Sessions: sessions, Turns: map[string]fixturekit.DemoTurn{}}
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	enforced := service.EnforceThreadsForTest(opts, service.Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}}})
	opts = enforced
	s, c := Start(t, opts)
	added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	WriteCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	return s, c, added.Project.ID, engine
}
