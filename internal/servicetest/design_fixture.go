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

// ListenWeb is the configuration that opens the web listener on a free
// loopback port.
const ListenWeb = "[listen]\nweb = \"127.0.0.1:0\"\n"

// DesignEnforcement is a root with a committed clone and the fake execution
// boundary that a design session's turns run through under production
// enforcement.
type DesignEnforcement struct {
	Options     service.Options
	Clone       string
	Engine      *fixturekit.DemoEngine
	Enforcement service.Enforcement
}

// PrepareDesignEnforcement writes before and after around the chief-of-staff
// role table of the root's configuration. The role runs in a container
// sandbox, the only mode every platform can enforce.
func PrepareDesignEnforcement(t *testing.T, before, after string) DesignEnforcement {
	t.Helper()
	opts, clone := ProjectFixture(t)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString(before)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString("[roles.chief_of_staff]\nsandbox = \"container\"\nimage = \"fixture-image\"\n" + after)
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
	return DesignEnforcement{Options: opts, Clone: clone, Engine: engine, Enforcement: service.Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}}}}
}

// AddDesignProject onboards clone as a project whose charter has one rule.
func AddDesignProject(t *testing.T, c *service.Client, clone string) config.ProjectID {
	t.Helper()
	added, err := c.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	WriteCharter(t, c, added.Project.ID, "1. Keep changes small.\n")
	return added.Project.ID
}

// NewDesignEnforcementFixture starts a service with production session
// enforcement on an onboarded project, opening the web listener when web is
// true.
func NewDesignEnforcementFixture(t *testing.T, web ...bool) (*service.Service, *service.Client, config.ProjectID, *fixturekit.DemoEngine) {
	t.Helper()
	before := ""
	if len(web) > 0 && web[0] {
		before = ListenWeb
	}
	d := PrepareDesignEnforcement(t, before, "")
	opts := service.EnforceThreadsForTest(d.Options, d.Enforcement)
	s, c := Start(t, opts)
	return s, c, AddDesignProject(t, c, d.Clone), d.Engine
}
