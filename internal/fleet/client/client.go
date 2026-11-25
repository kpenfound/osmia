// Package client is `osmia client`: a process that connects out to an Osmia
// server over the tailnet, takes model-session turns and runs them on its own
// machine.
//
// The server stays the control plane. A client holds a lease kept alive by
// heartbeats, checks which of the server's profiles its machine can run, builds
// each turn's view from a cached clone it fetches over the server's read-only
// git route, runs the session with the machine's own provider login and Dagger
// setting, forwards the sandbox's tool calls to the server, carries out the
// server's view and skill operations on its own files, and sends back the
// result and the finished view. It has no trace store, no telemetry and no
// configuration beyond its flags, logs to stderr only, and never writes to any
// git store.
package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/skills"
)

// Options configure a client. Server, Slots and Dir are its flags; the rest
// are boundaries a test replaces and the host's own are used when they are nil.
type Options struct {
	// Server is the base URL of the server's tailnet API.
	Server string
	// Slots is how many turns the client runs at once.
	Slots int
	// Dir is the client-local directory: its cached clones, its sessions, and the
	// views and results it has not yet delivered.
	Dir string
	// Interrupts delivers one value for each Ctrl+C or SIGTERM. The first starts
	// a drain, the second cancels the running sessions and sends what they
	// leave, and a third stops waiting for the server.
	Interrupts <-chan struct{}
	// Logger receives the client's own log; nil logs to stderr.
	Logger *slog.Logger

	// HTTP is the client the server is reached with.
	HTTP *http.Client
	// Engine runs sessions; nil is agentcore's, with skills prepared under Dir.
	Engine coreadapter.Engine
	// LookPath finds a tool and Command runs it, for the tool checks.
	LookPath func(string) (string, error)
	Command  func(ctx context.Context, path string, args ...string) (string, error)
	// Getenv reads this machine's environment, such as its Dagger setting.
	Getenv func(string) string
	// Now is the clock.
	Now func() time.Time
	// RetryEvery is how often an unsent view or result is tried again and how
	// long the client waits before it connects again; ExitWait is how long, after
	// a second interrupt, it waits for the server to take what it holds.
	RetryEvery, ExitWait time.Duration
}

// Client is a running client.
type Client struct {
	opts       Options
	log        *slog.Logger
	http       *http.Client
	base       string
	dir        string
	slots      int
	engine     coreadapter.Engine
	clones     *clones
	retryEvery time.Duration
	exitWait   time.Duration

	rootCtx context.Context
	wg      sync.WaitGroup
	recheck chan struct{}
	deliver chan struct{}

	mu          sync.Mutex
	lease       string
	connected   bool
	lastContact time.Time
	heartbeat   time.Duration
	grace       time.Duration
	poll        time.Duration
	ack         int64
	handout     api.ProfileHandout
	draining    bool
	hard        bool
	abandoned   bool
	running     map[string]*turn
	pending     map[string]*record
	finished    chan struct{}
	finishOnce  sync.Once
	forced      bool
}

// Run runs a client until it is drained, or until ctx ends. It returns nil after
// a drain, and an error when it cannot run at all, such as when the server
// speaks another protocol version.
func Run(ctx context.Context, o Options) error {
	c, err := newClient(o)
	if err != nil {
		return err
	}
	return c.run(ctx)
}

func newClient(o Options) (*Client, error) {
	u, err := url.Parse(o.Server)
	if err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("--server %q is not an http or https URL", o.Server)
	}
	if o.Slots < 1 {
		return nil, errors.New("--slots must be at least 1")
	}
	if o.Dir == "" {
		return nil, errors.New("the client needs a directory of its own")
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"clones", "stage", "turns", "sessions", "skills"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	c := &Client{opts: o, log: o.Logger, http: o.HTTP, base: strings.TrimRight(u.String(), "/"), dir: dir, slots: o.Slots, engine: o.Engine,
		retryEvery: o.RetryEvery, exitWait: o.ExitWait, recheck: make(chan struct{}, 1), deliver: make(chan struct{}, 1),
		running: map[string]*turn{}, pending: map[string]*record{}, finished: make(chan struct{}),
		heartbeat: 10 * time.Second, grace: 2 * time.Minute, poll: 20 * time.Second}
	if c.log == nil {
		c.log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	if c.retryEvery <= 0 {
		c.retryEvery = 5 * time.Second
	}
	if c.exitWait <= 0 {
		c.exitWait = 2 * time.Minute
	}
	if c.engine == nil {
		manager := skills.NewManager(filepath.Join(dir, "skills"))
		c.engine = coreadapter.CoreEngine{Runner: agent.Runner{Skills: manager, SkillMountDirs: []string{manager.Dir}}}
	}
	c.clones = newClones(filepath.Join(dir, "clones"), c.base)
	return c, nil
}

func (c *Client) now() time.Time {
	if c.opts.Now != nil {
		return c.opts.Now()
	}
	return time.Now()
}

func (c *Client) getenv(name string) string {
	if c.opts.Getenv != nil {
		return c.opts.Getenv(name)
	}
	return os.Getenv(name)
}

func (c *Client) lookPath(name string) (string, error) {
	if c.opts.LookPath != nil {
		return c.opts.LookPath(name)
	}
	return exec.LookPath(name)
}

func (c *Client) command(ctx context.Context, path string, args ...string) (string, error) {
	if c.opts.Command != nil {
		return c.opts.Command(ctx, path, args...)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return string(out), err
}

func (c *Client) touch() {
	c.mu.Lock()
	c.lastContact = c.now()
	c.mu.Unlock()
}

func (c *Client) currentLease() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected {
		return ""
	}
	return c.lease
}

func seconds(n int, fallback time.Duration) time.Duration {
	if n <= 0 {
		return fallback
	}
	return time.Duration(n) * time.Second
}

func (c *Client) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.rootCtx = ctx
	if err := c.loadPending(); err != nil {
		return err
	}
	go c.watchInterrupts(ctx)
	go c.watchGrace(ctx)
	go c.deliverLoop(ctx)
	go c.checkLoop(ctx)
	c.log.Info("client starting", "server", c.base, "slots", c.slots, "directory", c.dir)
	c.changed()
	err := c.connectLoop(ctx)
	c.cancelAll(errors.New("the client is exiting"))
	c.wg.Wait()
	c.leave()
	c.mu.Lock()
	left := len(c.pending)
	c.mu.Unlock()
	if left > 0 {
		c.log.Warn("turns the server has not answered for stay in the client's directory and are sent when it next connects", "turns", left, "directory", filepath.Join(c.dir, "turns"))
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return err
}

func (c *Client) leave() {
	lease := c.currentLease()
	if lease == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.post(ctx, api.ClientRoute(lease, "leave"), api.LeaveRequest{}, &api.LeaveResponse{}, 5*time.Second); err != nil {
		c.log.Warn("cannot tell the server this client is leaving; its lease ends by itself", "error", err)
	}
}

func (c *Client) connectLoop(ctx context.Context) error {
	for {
		select {
		case <-c.finished:
			return nil
		case <-ctx.Done():
			return nil
		default:
		}
		err := c.connect(ctx)
		if err == nil {
			c.serve(ctx)
		} else {
			if ctx.Err() != nil {
				return nil
			}
			var r *refusal
			switch {
			case refusedWith(err, api.VersionMismatch), refusedWith(err, api.Validation):
				errors.As(err, &r)
				return fmt.Errorf("the server refused this client: %s", r.Err.Message)
			case refusedWith(err, api.ClientConnected):
				errors.As(err, &r)
				c.log.Warn("another connection from this node holds a lease; connecting again", "detail", r.Err.Message)
			default:
				c.log.Warn("cannot connect to the server; trying again", "error", err)
			}
		}
		select {
		case <-c.finished:
			return nil
		case <-ctx.Done():
			return nil
		case <-time.After(c.retryEvery):
		}
	}
}

func (c *Client) connect(ctx context.Context) error {
	c.mu.Lock()
	req := api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: c.slots, Draining: c.draining, Lease: c.lease}
	for id := range c.running {
		req.Running = append(req.Running, id)
	}
	c.mu.Unlock()
	var resp api.ClientConnectResponse
	if err := c.post(ctx, api.ClientConnectPath, req, &resp, callTimeout); err != nil {
		return err
	}
	c.mu.Lock()
	c.lease, c.connected, c.abandoned = resp.Lease, true, false
	c.heartbeat, c.grace, c.poll = seconds(resp.HeartbeatSeconds, 10*time.Second), seconds(resp.GraceSeconds, 2*time.Minute), seconds(resp.PollSeconds, 20*time.Second)
	if !resp.Resumed {
		c.ack = 0
	}
	c.handout = resp.Profiles
	c.mu.Unlock()
	if resp.Resumed {
		c.log.Info("reconnected to the server", "lease", resp.Lease)
	} else {
		c.log.Info("connected to the server", "identity", resp.Client.Name)
	}
	for _, answer := range resp.Turns {
		switch answer.Answer {
		case api.TurnLost:
			c.cancelTurn(answer.Turn, "the server no longer holds the turn")
		case api.TurnReattached:
			c.mu.Lock()
			if t := c.running[answer.Turn]; t != nil && answer.Token != "" {
				t.setToken(answer.Token)
			}
			c.mu.Unlock()
			c.log.Info("reattached a running turn", "turn", answer.Turn)
		}
	}
	c.wakeCheck()
	c.kick()
	return nil
}

func (c *Client) setDisconnected() {
	c.mu.Lock()
	c.connected = false
	c.mu.Unlock()
}

// serve polls for messages and heartbeats until the connection fails or the
// client has finished. The sessions keep running when it returns.
func (c *Client) serve(ctx context.Context) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() { errc <- c.pollLoop(sctx) }()
	go func() { errc <- c.heartbeatLoop(sctx) }()
	var err error
	select {
	case err = <-errc:
	case <-c.finished:
	case <-ctx.Done():
	}
	cancel()
	<-errc
	if err != nil && ctx.Err() == nil {
		if refusedWith(err, api.LeaseEnded) {
			c.log.Warn("the server ended this client's lease; connecting again")
		} else {
			c.log.Warn("lost the connection to the server; running turns keep going", "error", err)
		}
		c.setDisconnected()
	}
}

func (c *Client) pollLoop(ctx context.Context) error {
	for ctx.Err() == nil {
		c.mu.Lock()
		lease, ack, wait := c.lease, c.ack, c.poll
		c.mu.Unlock()
		var resp api.PollResponse
		if err := c.post(ctx, api.ClientRoute(lease, "poll"), api.PollRequest{Ack: ack, WaitSeconds: int(wait / time.Second)}, &resp, wait+callTimeout); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, m := range resp.Messages {
			c.mu.Lock()
			seen := m.Seq <= c.ack
			if !seen {
				c.ack = m.Seq
			}
			c.mu.Unlock()
			if !seen {
				c.handle(m)
			}
		}
	}
	return nil
}

func (c *Client) heartbeatLoop(ctx context.Context) error {
	c.mu.Lock()
	every := c.heartbeat
	c.mu.Unlock()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		c.mu.Lock()
		lease, draining := c.lease, c.draining
		c.mu.Unlock()
		var resp api.HeartbeatResponse
		if err := c.post(ctx, api.ClientRoute(lease, "heartbeat"), api.HeartbeatRequest{Turns: c.progress()}, &resp, callTimeout); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, id := range resp.Lost {
			c.cancelTurn(id, "the server no longer holds the turn")
		}
		if draining && !resp.Draining {
			c.sendDrain(ctx, lease)
		}
	}
}

func (c *Client) sendDrain(ctx context.Context, lease string) {
	if err := c.post(ctx, api.ClientRoute(lease, "drain"), api.DrainRequest{}, &api.DrainResponse{}, callTimeout); err != nil && ctx.Err() == nil {
		c.log.Warn("cannot tell the server this client is draining yet", "error", err)
	}
}

func (c *Client) progress() []api.TurnProgress {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []api.TurnProgress{}
	for id, t := range c.running {
		out = append(out, api.TurnProgress{Turn: id, Phase: t.currentPhase()})
	}
	return out
}

func (c *Client) handle(m api.Message) {
	switch {
	case m.Kind == api.MessageProfiles && m.Profiles != nil:
		c.mu.Lock()
		c.handout = *m.Profiles
		c.mu.Unlock()
		c.log.Info("the server's configuration changed; checking what this client can run")
		c.wakeCheck()
	case m.Kind == api.MessageOffer && m.Offer != nil:
		go c.handleOffer(*m.Offer)
	case m.Kind == api.MessageCancel && m.Cancel != nil:
		c.cancelTurn(m.Cancel.Turn, cmpOr(m.Cancel.Reason, "the server cancelled the turn"))
	case m.Kind == api.MessageRequest && m.Request != nil:
		go c.handleRequest(*m.Request)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// watchGrace cancels the running sessions when the server has not been heard
// from for the grace window, at which point the server's lease has ended and it
// recovers the turns. Each mason's files are kept and sent when the server is
// back.
func (c *Client) watchGrace(ctx context.Context) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		c.mu.Lock()
		out := !c.abandoned && len(c.running) > 0 && !c.lastContact.IsZero() && c.now().Sub(c.lastContact) > c.grace
		if out {
			c.abandoned = true
		}
		c.mu.Unlock()
		if out {
			c.log.Warn("the server has not answered within the grace window; cancelling the running turns and keeping their files")
			c.cancelAll(errors.New("the server could not be reached within the grace window"))
		}
	}
}

func (c *Client) watchInterrupts(ctx context.Context) {
	count := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.opts.Interrupts:
			count++
			switch count {
			case 1:
				c.startDrain()
			case 2:
				c.startHardStop()
			default:
				c.forceExit()
			}
		}
	}
}

// startDrain takes no new turns, tells the server, and ends the client once
// its running turns have been sent.
func (c *Client) startDrain() {
	c.mu.Lock()
	if c.draining {
		c.mu.Unlock()
		return
	}
	c.draining = true
	list := c.runningList()
	lease := ""
	if c.connected {
		lease = c.lease
	}
	c.mu.Unlock()
	c.log.Info("draining: taking no new turns, finishing the running ones", "running", len(list))
	for _, line := range list {
		c.log.Info("waiting for a running turn", "turn", line)
	}
	if lease != "" {
		go c.sendDrain(c.rootCtx, lease)
	}
	c.changed()
}

func (c *Client) runningList() []string {
	var out []string
	for id, t := range c.running {
		out = append(out, fmt.Sprintf("%s (%s, %s)", id, t.offer.Role, t.currentPhase()))
	}
	return out
}

// startHardStop cancels the running sessions, and still sends each one's files.
func (c *Client) startHardStop() {
	c.startDrain()
	c.mu.Lock()
	c.hard = true
	c.mu.Unlock()
	c.log.Warn("cancelling the running turns and sending their files")
	c.cancelAll(errors.New("the client was interrupted"))
	time.AfterFunc(c.exitWait, c.forceExit)
}

func (c *Client) forceExit() {
	c.mu.Lock()
	c.forced = true
	c.mu.Unlock()
	c.finish()
}

func (c *Client) finish() { c.finishOnce.Do(func() { close(c.finished) }) }

// changed ends a draining client once nothing runs and nothing waits to be
// sent.
func (c *Client) changed() {
	select {
	case <-c.finished:
		return
	default:
	}
	c.mu.Lock()
	done := c.draining && len(c.running) == 0 && len(c.pending) == 0
	c.mu.Unlock()
	if done {
		c.log.Info("drained: every turn has been sent")
		c.finish()
	}
}
