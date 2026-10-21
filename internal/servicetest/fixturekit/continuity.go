package fixturekit

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest/enforcertest"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	DemoAgent  = "agent_mason"
	DemoThread = "thread_mason"
	DemoRole   = "mason"
	// Delivery credentials exist in the service process but never reach a turn.
	DemoSecret = "ghp_demo_delivery_credential_must_not_leak"
)

var DemoStart = time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)

// DemoTimeout only guards against a hang; trace publication runs Git and is
// slow under the race detector on a loaded machine.
const DemoTimeout = 5 * time.Minute

type DemoClock struct {
	Mu sync.Mutex
	At time.Time
}

func (c *DemoClock) Now() time.Time {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.At = c.At.Add(time.Second)
	return c.At
}

type DemoLease func(context.Context) error

func (f DemoLease) Release(ctx context.Context) error { return f(ctx) }

// DemoWorkspaces is the fake repository provider: it lends the target clone,
// which keeps its service-owned .git directory, to the turn preparation path.
type DemoWorkspaces struct {
	Mu                 sync.Mutex
	Directory          string
	Acquired, Released int
}

func (w *DemoWorkspaces) Acquire(_ context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	w.Acquired++
	release := DemoLease(func(context.Context) error { w.Mu.Lock(); w.Released++; w.Mu.Unlock(); return nil })
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{ID: "clone", Directory: w.Directory, Access: req.Access}, Lease: release}, nil
}

// DemoTransport serves the scoped MCP server in memory and lends the client
// side to the fake container engine under the turn's service token.
type DemoTransport struct {
	Sessions *DemoSessions
}

type DemoSessions struct {
	Mu    sync.Mutex
	ByKey map[string]*mcp.ClientSession
}

func (d *DemoTransport) Start(ctx context.Context, server *mcp.Server) (coreadapter.Endpoint, coreadapter.Lease, error) {
	serverSide, clientSide := mcp.NewInMemoryTransports()
	served, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		return coreadapter.Endpoint{}, nil, err
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "fake-agent", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		served.Close()
		return coreadapter.Endpoint{}, nil, err
	}
	token := rand.Text()
	d.Sessions.Mu.Lock()
	d.Sessions.ByKey[token] = client
	d.Sessions.Mu.Unlock()
	release := DemoLease(func(context.Context) error {
		d.Sessions.Mu.Lock()
		delete(d.Sessions.ByKey, token)
		d.Sessions.Mu.Unlock()
		return errors.Join(client.Close(), served.Wait())
	})
	return coreadapter.Endpoint{URL: "http://osmia-mcp.invalid/turn", BearerTokenEnvironment: "OSMIA_MCP_TOKEN", Token: token}, release, nil
}

// DemoTurn is what the fake backend does inside the boundary core verified.
type DemoTurn func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error)

// DemoEngine is the fake execution engine. Core's fake enforcer admits each
// Request the way a real session does, and Run verifies it with core's own
// container boundary, which is evidence of the grants, not of OS enforcement.
type DemoEngine struct {
	Mu       sync.Mutex
	Sessions *DemoSessions
	Turns    map[string]DemoTurn
	Resume   func(previous, next coreadapter.Profile, session coreadapter.BackendSession) error
	Runs     []string
	Checks   int
}

func (e *DemoEngine) CheckResume(_ context.Context, previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
	e.Mu.Lock()
	defer e.Mu.Unlock()
	e.Checks++
	return e.Resume(previous, next, session)
}

func (e *DemoEngine) Enforcer(s coreadapter.ExecutionSettings) (agent.Enforcer, error) {
	return &enforcertest.Enforcer{Sandbox: s.Mode, Image: s.Image, Agent: func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
		return e.Run(ctx, turn.Request)
	}}, nil
}

func (e *DemoEngine) Verify(req agent.Request) (*agent.Turn, error) {
	return (&agent.Runner{}).Verify(req)
}

func (e *DemoEngine) Run(ctx context.Context, req agent.Request) (*agent.Result, error) {
	verified, err := e.Verify(req)
	if err != nil {
		return nil, err
	}
	e.Mu.Lock()
	e.Runs = append(e.Runs, req.Name)
	turn := e.Turns[req.Name]
	if turn == nil {
		// Turns the service names itself, such as event turns, share a script.
		turn = e.Turns["*"]
	}
	e.Mu.Unlock()
	e.Sessions.Mu.Lock()
	tools := e.Sessions.ByKey[req.Env["OSMIA_MCP_TOKEN"]]
	e.Sessions.Mu.Unlock()
	// A session turn is granted no tool, so it has no MCP host to lend it
	// one either, the same way a classifier turn has none.
	if turn == nil || (tools == nil && req.Profile.Name != "classifier" && req.Profile.Name != SessionRole) {
		return nil, fmt.Errorf("unexpected turn %q", req.Name)
	}
	return turn(ctx, req, verified, tools)
}

func CallTool(ctx context.Context, tools *mcp.ClientSession, name string, args map[string]any) (string, error) {
	result, err := tools.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, content := range result.Content {
		if c, ok := content.(*mcp.TextContent); ok {
			text.WriteString(c.Text)
		}
	}
	if result.IsError {
		return "", errors.New(text.String())
	}
	return text.String(), nil
}

func DemoGit(t *testing.T, home string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
