package intakefixture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/plan"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const architectContainer = `[roles.architect]
sandbox = "container"
image = "fixture-image"
`

// ArchitectExecution prepares a local clone and fake architect execution boundaries.
func ArchitectExecution(t *testing.T, cfg config.Options, clone string) (*fixturekit.DemoEngine, *fixturekit.DemoSessions, *fixturekit.DemoClock, *intake.Architect, *fixturekit.FakeIssues) {
	t.Helper()

	home := filepath.Dir(clone)
	fixturekit.ConfigureDeliverySigner(t, clone)
	configFile, err := os.OpenFile(filepath.Join(cfg.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString(architectContainer)
	fixturekit.Must(t, errors.Join(err, configFile.Close()))
	fixturekit.Must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "CODEOWNERS"), []byte("/internal/ @core\n"), 0600))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "secret.env"), []byte("TOKEN="+fixturekit.DemoSecret+"\n"), 0600))
	fixturekit.DemoGit(t, home, "-C", clone, "add", "internal", "CODEOWNERS")
	fixturekit.DemoGit(t, home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "base")
	sessions := &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}
	engine := &fixturekit.DemoEngine{Sessions: sessions, Turns: map[string]fixturekit.DemoTurn{}}
	engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error {
		return coreadapter.ErrResumeUnavailable
	}
	clock := &fixturekit.DemoClock{At: fixturekit.DemoStart}
	issues := &fixturekit.FakeIssues{Token: "ghp_architect_secret"}
	architect := &intake.Architect{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: sessions}}}
	return engine, sessions, clock, architect, issues
}

// ArchitectService is the running service lifecycle used by the fixture adapter.
type ArchitectService interface {
	Socket() string
	Wait() error
}

func StartArchitect[S ArchitectService, C any](t *testing.T, start func(context.Context) (S, error), client func(string) C, configure func(C)) (S, C, chan struct{}) {
	t.Helper()
	s, err := start(context.Background())
	fixturekit.Must(t, err)
	c := client(s.Socket())
	configure(c)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Wait(); err != nil {
			t.Errorf("service stopped: %v", err)
		}
	}()
	return s, c, done
}

func StopArchitect(t *testing.T, closeClient func(), closeService func() error, done <-chan struct{}) {
	t.Helper()
	closeClient()
	err := closeService()
	<-done
	fixturekit.Must(t, err)
}

type ArchitectProjectClient interface {
	AddProject(context.Context, api.ProjectAddRequest) (api.ProjectResponse, error)
}

func AddArchitectProject(t *testing.T, client ArchitectProjectClient, clone, charter string, writeCharter func(*testing.T, config.ProjectID, string)) (config.ProjectID, string) {
	t.Helper()
	added, err := client.AddProject(context.Background(), fixturekit.Request(clone))
	fixturekit.Must(t, err)
	writeCharter(t, added.Project.ID, charter)
	return added.Project.ID, added.Project.Trace
}

func ConfigureDebate(t *testing.T, root string, members, rounds int, extra string) {
	t.Helper()
	configFile, err := os.OpenFile(filepath.Join(root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = fmt.Fprintf(configFile, "[roles.committee]\nsandbox = \"container\"\nimage = \"fixture-image\"\n[capacity]\ncommittee = %d\n[shed]\nmax_rounds = %d\n%s", members, rounds, extra)
	fixturekit.Must(t, errors.Join(err, configFile.Close()))
}

func (f *ArchitectScenario) ScriptDebate(rounds int) {
	f.Script("draft-1-1", map[string]string{plan.SpecPath: ValidSpec, plan.PlanPath: ValidPlan}, nil)
	for round := 1; round <= rounds; round++ {
		f.Script(intake.ReplyTurnID(round, 1), nil, nil)
	}
}
