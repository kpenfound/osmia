package clientjourney

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest/enforcertest"
	"github.com/kpenfound/agentcore/mcphost"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/fleet/client"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const nodeHeader = "X-Journey-Node"

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stopwatch is the clock the server's client leases run on, which a test moves
// to end a lease without waiting for the grace window.
type stopwatch struct {
	mu sync.Mutex
	at time.Time
}

func (c *stopwatch) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *stopwatch) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// network is the client machine's path to the server. It stands in for the
// tailnet: it names the caller's node to the server, and a test can cut it.
type network struct {
	node string
	down atomic.Bool
	base http.RoundTripper
}

func (n *network) RoundTrip(r *http.Request) (*http.Response, error) {
	if n.down.Load() {
		return nil, errors.New("the network is cut")
	}
	r = r.Clone(r.Context())
	r.Header.Set(nodeHeader, n.node)
	return n.base.RoundTrip(r)
}

// bothWays serves each turn's tools over the HTTP endpoint a client reaches
// through the relay, and in memory to the fake sessions that run on the
// server's own machine.
type bothWays struct {
	http     coreadapter.CoreTransport
	sessions *fixturekit.DemoSessions
}

func (b bothWays) Start(ctx context.Context, server *mcp.Server) (coreadapter.Endpoint, coreadapter.Lease, error) {
	endpoint, lease, err := b.http.Start(ctx, server)
	if err != nil {
		return coreadapter.Endpoint{}, nil, err
	}
	serverSide, clientSide := mcp.NewInMemoryTransports()
	served, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		return coreadapter.Endpoint{}, nil, errors.Join(err, lease.Release(ctx))
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "fake-agent", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		_ = served.Close()
		return coreadapter.Endpoint{}, nil, errors.Join(err, lease.Release(ctx))
	}
	b.sessions.Mu.Lock()
	b.sessions.ByKey[endpoint.Token] = session
	b.sessions.Mu.Unlock()
	return endpoint, fixturekit.DemoLease(func(ctx context.Context) error {
		b.sessions.Mu.Lock()
		delete(b.sessions.ByKey, endpoint.Token)
		b.sessions.Mu.Unlock()
		return errors.Join(session.Close(), served.Wait(), lease.Release(ctx))
	}), nil
}

// machine is a client machine: the real `osmia client` code on its own
// directory, with a fake agent in place of the model session.
type machine struct {
	t          *testing.T
	node       string
	dir        string
	net        *network
	logs       *syncBuffer
	interrupts chan struct{}
	script     func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error)
	cancel     context.CancelFunc
	done       chan error
}

func (m *machine) Enforcer(settings coreadapter.ExecutionSettings) (agent.Enforcer, error) {
	return &enforcertest.Enforcer{Sandbox: settings.Mode, Image: settings.Image, SandboxAgent: settings.Agent,
		Agent: func(ctx context.Context, turn *enforcertest.Turn) (*agent.Result, error) { return m.script(ctx, turn) }}, nil
}

func (m *machine) SkillDirs() []string { return nil }

func (m *machine) PrepareSkills(_ context.Context, refs []string) ([]string, error) {
	if len(refs) != 0 {
		return nil, errors.New("this machine prepares no skills")
	}
	return nil, nil
}

// start runs the client with slots slots against the server at address, until
// it exits.
func (m *machine) start(address string, slots int) {
	m.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel, m.done = cancel, make(chan error, 1)
	opts := client.Options{
		Server: "http://" + address, Slots: slots, Dir: m.dir, Interrupts: m.interrupts,
		Logger: slog.New(slog.NewTextHandler(m.logs, nil)),
		HTTP:   &http.Client{Transport: m.net}, Engine: m,
		RetryEvery: 200 * time.Millisecond, ExitWait: 5 * time.Second,
		LookPath: func(name string) (string, error) { return "/machine/" + name, nil },
		Command: func(_ context.Context, path string, _ ...string) (string, error) {
			return filepath.Base(path) + " 1.0\n", nil
		},
		Getenv: func(string) string { return "" },
	}
	go func() { m.done <- client.Run(ctx, opts) }()
	m.t.Cleanup(m.stop)
}

func (m *machine) stop() {
	if m.cancel == nil {
		return
	}
	m.cancel()
	select {
	case <-m.done:
	case <-time.After(20 * time.Second):
		m.t.Error("the client did not stop")
	}
	m.cancel = nil
	if m.t.Failed() {
		m.t.Log(m.logs.String())
	}
}

// exited waits for the client to return and reports its error.
func (m *machine) exited(within time.Duration) error {
	m.t.Helper()
	select {
	case err := <-m.done:
		m.cancel = nil
		return err
	case <-time.After(within):
		m.t.Fatal("the client did not exit")
		return nil
	}
}

// interrupt is a Ctrl+C on the client's terminal.
func (m *machine) interrupt() { m.interrupts <- struct{}{} }

// dial connects to the tools of the turn through the MCP endpoint the client
// opened for its sandbox. The fake agent stands in for the sandbox, which
// resolves the host name a container reaches its host by.
func dial(ctx context.Context, req agent.Request) (*mcp.ClientSession, error) {
	entry, ok := req.Profile.MCP["osmia_0"]
	if !ok {
		return nil, errors.New("the turn was given no tool endpoint")
	}
	endpoint := strings.Replace(entry.URL, coreadapter.ContainerHost, "127.0.0.1", 1)
	token := req.Env[coreadapter.TokenEnvironment]
	return mcp.NewClient(&mcp.Implementation{Name: "fake-agent", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: mcphost.BearerTransport(token, nil)}, MaxRetries: -1}, nil)
}

// serverRig is a running service with a fake tailnet, and the clients that
// connect to it.
type serverRig struct {
	t      *testing.T
	f      *journey.ShedFixture
	masons *intakefixture.FakeMasons
	leases *stopwatch
}

// fixtureBuilder starts the mason fixture with prepare applied to its options.
type fixtureBuilder func(t *testing.T, prepare func(*journey.Options)) (*journey.ShedFixture, *intakefixture.FakeMasons)

// single is a fixture whose capacity table holds capacity and whose plan is the
// sealed plan the test builds.
func single(capacity, plan string) fixtureBuilder {
	return func(t *testing.T, prepare func(*journey.Options)) (*journey.ShedFixture, *intakefixture.FakeMasons) {
		return journey.NewMasonFixtureWith(t, config.WorkspacesJujutsu, capacity, plan, prepare)
	}
}

// parallel is a fixture with masons mason slots and perWorkstream masons at
// once in a workstream, which plays the first turn of every unit of plan.
func parallel(masons, perWorkstream int, plan string) fixtureBuilder {
	return func(t *testing.T, prepare func(*journey.Options)) (*journey.ShedFixture, *intakefixture.FakeMasons) {
		return journey.NewParallelMasonFixturePrepared(t, config.WorkspacesJujutsu, masons, perWorkstream, plan, prepare)
	}
}

// newServerRig starts the service from build. Its masons work in Jujutsu
// workspaces, whose starting revisions are commits a client fetches through the
// git route. The service listens on a fake tailnet node, names each caller by
// the node header, and serves its tools both over HTTP and in memory.
func newServerRig(t *testing.T, build fixtureBuilder) *serverRig {
	t.Helper()
	intakefixture.RequireJJ(t)
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = network ] && [ \"$2\" = inspect ]; then echo 127.0.0.1; exit 0; fi\nexit 1\n"
	fixturekit.Must(t, os.WriteFile(filepath.Join(bin, agent.ContainerEngine), []byte(script), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := &serverRig{t: t, leases: &stopwatch{at: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}}
	r.f, r.masons = build(t, func(opts *journey.Options) {
		fixturekit.WithListen(t, opts.Config.Root, "tailnet = \"osmia\"\n")
		join, _ := fixturekit.TailnetJoiner("osmia-1")
		opts.JoinTailnet = func(hostname, dir string) (service.TailnetListener, error) { return join(hostname, dir) }
		opts.ClientIdentity = func(req *http.Request) (fleet.Identity, error) {
			id := req.Header.Get(nodeHeader)
			if id == "" {
				return fleet.Identity{}, errors.New("the request names no node")
			}
			return fleet.Identity{ID: id, Name: "name-of-" + id}, nil
		}
		opts.Fleet = fleet.Options{Now: r.leases.Now, Grace: 3 * time.Second, Heartbeat: time.Second, Poll: time.Second}
		sessions := opts.Architect.Hosts.(*coreadapter.MCPHost).Transport.(*fixturekit.DemoTransport).Sessions
		hosts := &coreadapter.MCPHost{Transport: bothWays{sessions: sessions}}
		enforced := journey.Enforce(*opts, service.Enforcement{Engine: opts.Committee.Engine, Hosts: hosts})
		opts.Threads, opts.Controls = enforced.Threads, enforced.Controls
	})
	r.f.Engine.Mu.Lock()
	r.f.Engine.Resume = func(coreadapter.Profile, coreadapter.Profile, coreadapter.BackendSession) error { return nil }
	r.f.Engine.Mu.Unlock()
	return r
}

func (r *serverRig) address() string { return r.f.S.TailnetAddr() }

// machine returns a client machine for the node.
func (r *serverRig) machine(node string) *machine {
	return &machine{t: r.t, node: node, dir: r.t.TempDir(), net: &network{node: node, base: &http.Transport{DisableKeepAlives: true}}, logs: &syncBuffer{}, interrupts: make(chan struct{}, 4)}
}

// connected is the client of the node as the server's registry holds it.
func (r *serverRig) connected(node string) (fleet.Client, bool) { return r.f.S.Fleet().Client(node) }

// awaitCapable waits until the node's client has reported that it can run a
// profile.
func (r *serverRig) awaitCapable(node string) fleet.Client {
	r.t.Helper()
	var c fleet.Client
	intakefixture.Soon(r.t, "client "+node+" to report the profiles it can run", func() bool {
		var ok bool
		c, ok = r.connected(node)
		if !ok || c.Capabilities == nil {
			return false
		}
		for _, p := range c.Capabilities.Profiles {
			if p.Runnable {
				return true
			}
		}
		return false
	})
	return c
}
