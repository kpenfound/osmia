package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	corejev "github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/eventstream"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/skills"
	"github.com/kpenfound/osmia/internal/tailnet"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

type Options struct {
	reviewChecks ReviewChecks
	// boundary injects interruptions using the current service lifetime. It is
	// bound before controllers launch so startup reconciliation is covered.
	boundary func(context.Context, string) error
	Config   config.Options
	Build    Identity
	// Workstreams supplies known identities when no trace exists. An existing
	// trace supplies its validated workstream manifests.
	Workstreams       []config.WorkstreamID
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	Reconciliation    reconcile.Options
	// Threads binds the runner-boundary reconciler to the trace this service
	// owns and the configuration it has loaded. It is called each time a
	// project's trace opens, at startup and when a project is added, and
	// replaces any runner adapter in Reconciliation.
	// With Threads set, the service also dispatches every queued workstream turn
	// on its own, never more than one turn per thread in flight and within the
	// configured capacity, replacing Reconciliation.Schedule. A runtime pause
	// holds new turns on the threads it covers, except chief-of-staff turns,
	// and a hard pause also stops the turns running on them; clearing it lets
	// them run. Outbox events are delivered to each
	// workstream's chief of staff as queued turns, one per event window, and
	// each recorded answer to a question is queued on its asker's thread.
	// The mason controller starts ready units of building workstreams and
	// final-review follow-ups of assembled workstreams within capacity, choosing
	// ready units disjoint from implementing and waiting units of their workstream,
	// and queues each started unit's first mason turn for the scheduler. It
	// parks a unit in waiting when its mason asks, and resumes it once the
	// answer is queued as the mason's next turn. The reviewer controller
	// dispatches exact-candidate reviews through durable reviewer threads,
	// parks reviewer questions, and applies completed verdicts and owner
	// rulings before the mason controller starts more work.
	// Callers must not close the repository.
	Threads func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error)
	// Librarian supplies the execution boundary of the librarian's
	// knowledge-base extraction and refresh turns. Without it those operations
	// fail with a recorded reason and the project stays usable.
	Librarian *Librarian
	// Architect supplies the execution boundary of the architect's drafting
	// turns, of its replies to shed rounds and of the redrafts the owner asks
	// for. Without it no draft, reply and redraft is requested, one already
	// requested stays pending, and handed workstreams, heard rounds and
	// requested redrafts wait.
	Architect *Architect
	// Committee supplies the execution boundary of the committee's shed
	// turns and final reviews. Without it no round or final review is
	// requested, one already requested stays pending, and sketched
	// workstreams, replied rounds and assembled workstreams wait; only a
	// workstream whose debate the owner skipped at hand-in enters the shed,
	// without a committee. The architect's replies and the
	// debate's conclusion need no committee runner.
	Committee *Committee
	// Issues fetches issue URLs handed in. It defaults to the GitHub REST API
	// with the service's GITHUB_TOKEN environment variable, which no session
	// receives.
	Issues issues.Client
	// PullRequests finds and opens the pull requests that deliver approved
	// workstreams. It defaults to the GitHub REST API with the service's
	// GITHUB_TOKEN environment variable, which no session receives.
	PullRequests pulls.Client
	// JevProvider returns the Jev provider for the current settings and API
	// key. It defaults to the TypeSafe-compatible HTTP API the settings name.
	JevProvider func(corejev.Settings, string) corejev.Provider
	// JoinTailnet joins the owner's tailnet when listen.tailnet is set. It
	// defaults to embedded Tailscale.
	JoinTailnet JoinTailnet
	// TailnetRetry is how long the service waits before it joins the tailnet
	// again after a failed join or a dropped listener. It defaults to 5
	// seconds.
	TailnetRetry time.Duration
	// NotifyRetry is how long the service waits before it posts a
	// notification the webhook refused again, doubled after each further
	// failure. It defaults to 30 seconds.
	NotifyRetry time.Duration
	// Location is the service host's time zone, whose calendar days the daily
	// budget counts. It defaults to the host's local time zone.
	Location *time.Location
	// Logger is the structured logger every line the service writes to its
	// standard output and standard error goes through: the service's own
	// records, embedded Tailscale's and the GitHub activity log. It defaults
	// to a JSON handler on os.Stderr.
	Logger *slog.Logger
	// controls is set by Enforce so the chief-of-staff tools it binds reach
	// the runtime state of the service started with these options.
	controls *runtimeControls
	// skills is the skill cache of the enforcement's engine; the service
	// gives it the loaded refresh policy.
	skills *skills.Manager
	// notifyInbox replaces the inbox the notifier reads, for tests.
	notifyInbox func(context.Context) (InboxResponse, *APIError)
	// checkJJ replaces the check of the jj on PATH that picks the workspace
	// backend of new workstreams, for tests.
	checkJJ jjCheck
	// sbxBin replaces the sbx binary reapSandboxes removes interrupted
	// sessions' Docker Sandboxes with, for tests. It defaults to
	// agent.SandboxCLI.
	sbxBin string
	// conversationTicks replaces retry and gate polling in conversation-path
	// fixtures. Enqueues and completions still wake the path directly.
	conversationTicks <-chan time.Time
	// schedulePassed is called, if set, once every time a reconciliation
	// pass's schedule stage completes without error - the stage that runs
	// the scheduler's dispatch pass among its hooks. Nil runs no hook. Tests
	// use it to wait for a pass to demonstrably complete instead of a fixed
	// number of ticks or a sleep.
	schedulePassed func()
}

// activeProject is the runtime state of one active project: its open trace
// and its reconciliation and conversation paths.
type activeProject struct {
	id            config.ProjectID
	repository    *trace.Repository
	controller    *reconcile.Controller
	conversations *conversationPath
	pipeline      *pipeline
	cancel        context.CancelFunc
	pollCancel    context.CancelFunc
	done          chan error
}

type Service struct {
	conversationSignal conversationSignal
	memory             hearsay.Health
	// jev answers the Jev judgments of every project's turns under the
	// current configuration.
	jev *jev.Judge
	mu  sync.Mutex // guards cfg, projects, pending and reloadErr
	cfg *config.Config
	// projects holds the runtime of every active project that has a trace, in
	// active_projects order.
	projects []*activeProject
	// pool is the role capacity the schedulers of every project draw on.
	pool          *scheduler.Shared
	gitOperations gitOperationPool
	checkSlots    *checkSlotPool
	// reviewSlots keeps each admitted review's committee reservation reachable
	// while its workflow operation waits for check capacity.
	reviewSlots sync.Map // operation ID -> *reviewReservation
	// spends is the daily spend each project's daily budget pass publishes.
	spends    daySpends
	pending   error
	reloadErr *ReloadError
	projectMu sync.Mutex // serializes project registration, removal and reload
	handInMu  sync.Mutex // serializes hand-ins
	sessionMu sync.Mutex // serializes session opens
	turns     runningTurns
	options   Options
	store     *runtime.Store
	logger    *slog.Logger
	lock      *os.File
	listener  *net.UnixListener
	web       net.Listener  // nil unless listen.web is configured
	tailnet   *tailnet.Link // nil unless listen.tailnet is configured
	// tailnetDone closes when the tailnet supervisor has stopped.
	tailnetDone chan struct{}
	socketInfo  os.FileInfo
	server      *http.Server
	lifetime    context.Context
	cancel      context.CancelFunc
	failures    chan error
	done        chan struct{}
	err         error
	ready       atomic.Bool
	requests    sync.WaitGroup
	boundary    func(string) error
	// hub carries change notifications to the client event stream.
	hub *eventstream.Hub
	// activity is the in-process record of long-running backend operations,
	// separate from the trace and the runtime store: reporting and reading
	// it never wait on the trace mutex or on operationMu.
	activity *activity.Registry
	// notifier posts new inbox entries to notify.webhook.
	notifier *notifier
	// driftAsked is set once an owner's drift rebase request is recorded,
	// so the next pass reads the drift schedule.
	driftAsked atomic.Bool
	// polls keeps attempt timing across stage rebuilds on reload.
	polls pullRequestAttempts
	// statusViews holds the last workstream list statusList produced, so a
	// request that cannot refresh it within coreReadBound still answers
	// with that snapshot, marked stale, instead of blocking past the
	// bound (bounded_read.go, readview.Bounded).
	statusViews readview.Cache[StatusResponse]
}

// log returns s's structured logger, or a silent one when a test built s
// directly without one: a missing logger never fails or panics the action
// it would have recorded.
func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return ghlog.Logger(context.Background())
}

// Start loads state and binds before returning. Wait joins shutdown and cleanup.
// A configuration without an active project starts an idle service that accepts
// project registration; an interrupted registration is completed first. Every
// active project gets its own runtime: its trace, its reconciliation loop and
// its restart recovery. Their schedulers share one pool of role capacity.
func Start(ctx context.Context, opts Options) (_ *Service, err error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	opts.Logger = logger
	ctx = ghlog.WithLogger(ctx, logger)
	root, err := opts.Config.Resolve()
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root.String(), ".service.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("open root ownership lock: %w", err)
	}
	defer func() {
		if err != nil {
			unlock(lock)
		}
	}()
	info, err := lock.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("root ownership lock must be a regular file")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("root is already owned by another Osmia service or cannot be locked; stop that service before starting another: %w", err)
	}
	opts.Config = root.Options(opts.Config.Home)
	if opts.Issues == nil {
		opts.Issues = issues.GitHub{Token: os.Getenv("GITHUB_TOKEN"), HTTP: &http.Client{Timeout: 30 * time.Second}}
	}
	if opts.PullRequests == nil {
		opts.PullRequests = pulls.GitHub{Token: os.Getenv("GITHUB_TOKEN"), HTTP: &http.Client{Timeout: 30 * time.Second}}
	}
	cfg, err := config.Load(opts.Config)
	if err != nil {
		return nil, err
	}
	s := &Service{options: opts, lock: lock, failures: make(chan error, 1), done: make(chan struct{}), hub: eventstream.New(), logger: logger}
	s.activity = activity.New(s.now)
	s.activity.Notify = func() { s.hub.Publish(Event{Kind: EventActivity}) }
	s.pool = &scheduler.Shared{Changed: func() { s.hub.Publish(Event{Kind: EventCapacity}) }}
	s.checkSlots = &checkSlotPool{limit: cfg.Capacity.Checks, changed: func() { s.hub.Publish(Event{Kind: EventCapacity}) }}
	// Getenv reads the Jev boost's API key the same way the default would,
	// and registers whatever value it reads for redaction: the owner may
	// point api_key_env at any environment variable name, not only
	// EnvSecrets' default, and the key must never reach a log record
	// whichever name configures it.
	s.jev = &jev.Judge{Config: func() config.Jev { return s.current().Jev }, Provider: opts.JevProvider, Getenv: func(name string) string {
		value := os.Getenv(name)
		redact.Register(value)
		return value
	}}
	if opts.skills != nil {
		opts.skills.Refresh = func() string { return s.current().Skills.Refresh }
	}
	if opts.controls != nil {
		opts.controls.service.Store(s)
	}
	pendingRecovery := s.StartActivity("recovery-pending", ActivityRecovery, "", "", "recovering an interrupted project registration")
	cfg, s.pending = s.recoverPending(ctx, cfg)
	pendingRecovery.Finish(s.pending)
	projects, err := s.openAll(cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			for _, p := range projects {
				p.repository.Close()
			}
		}
	}()
	inputs, err := s.runtimeInputs(cfg, projects)
	if err != nil {
		return nil, err
	}
	st, _, err := runtime.Open(inputs)
	if err != nil {
		return nil, err
	}
	st.Observe(func() { s.hub.Publish(Event{Kind: EventRuntime}) })
	defer func() {
		if err != nil {
			st.Close()
		}
	}()
	if err = s.reconcileSessionDestroys(ctx, cfg, projects, st); err != nil {
		return nil, err
	}
	state, _ := st.Snapshot()
	if err = s.ensureWorkgroups(ctx, cfg, projects, state.Archived); err != nil {
		return nil, err
	}
	if err = removeStaleSocket(cfg.Listen.Socket); err != nil {
		return nil, err
	}
	// Protect the socket during bind, before its own mode can be restricted.
	if err = os.Chmod(root.String(), 0700); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.Listen.Socket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	info, err = os.Lstat(cfg.Listen.Socket)
	if err != nil {
		listener.Close()
		return nil, err
	}
	s.cfg, s.projects, s.store, s.listener, s.socketInfo = cfg, projects, st, listener, info
	for _, p := range projects {
		s.installMemberLifecycle(p.repository)
	}
	if err = os.Chmod(cfg.Listen.Socket, 0600); err != nil {
		s.cleanupSocket()
		return nil, err
	}
	if cfg.Listen.Web != "" {
		if s.web, err = net.Listen("tcp", cfg.Listen.Web); err != nil {
			s.cleanupSocket()
			return nil, fmt.Errorf("bind listen.web %s: %w", cfg.Listen.Web, err)
		}
	}
	if cfg.Listen.Tailnet != "" {
		s.tailnet, s.tailnetDone = &tailnet.Link{}, make(chan struct{})
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 5 * time.Second
	}
	if opts.NotifyRetry <= 0 {
		opts.NotifyRetry = 30 * time.Second
	}
	if opts.TailnetRetry <= 0 {
		opts.TailnetRetry = 5 * time.Second
	}
	if opts.ReadHeaderTimeout <= 0 {
		opts.ReadHeaderTimeout = 5 * time.Second
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = 10 * time.Second
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 10 * time.Second
	}
	s.options = opts
	s.recoverArchiveLifecycle(ctx)
	if s.tailnet != nil {
		// A first join that fails leaves the tailnet down for the supervisor
		// to retry; it never fails startup.
		if node, err := s.joinTailnet(root, cfg.Listen.Tailnet); err != nil {
			s.tailnet.Set(nil, err.Error())
			logger.Warn("tailnet join failed; the supervisor will retry", "component", "tailnet", "hostname", cfg.Listen.Tailnet, "error", redact.Secrets(err.Error(), redact.KnownSecrets()...))
		} else {
			s.tailnet.Set(node, "")
			logger.Info("tailnet joined", "component", "tailnet", "hostname", cfg.Listen.Tailnet, "state", "up")
		}
	}
	for _, p := range projects {
		view := cfg.For(p.id)
		workspaces := s.StartActivity("recovery-workspaces-"+string(p.id), ActivityRecovery, p.id, "", "recovering interrupted workspace operations")
		err = s.recoverWorkspaces(ctx, view)
		workspaces.Finish(err)
		if err != nil {
			s.cleanupSocket()
			st.Close()
			return nil, fmt.Errorf("recover interrupted workspace operations: %w", err)
		}
		sessions := s.StartActivity("recovery-sessions-"+string(p.id), ActivityRecovery, p.id, "", "recovering thread sessions")
		err = s.recoverSessions(ctx, view, p.repository)
		sessions.Finish(err)
		if err != nil {
			s.cleanupSocket()
			st.Close()
			return nil, fmt.Errorf("recover thread sessions: %w", err)
		}
		reapSandboxes(ctx, view, s.options.sbxBin)
	}
	if err = s.recoverWorkgroupChiefSessions(ctx, projects); err != nil {
		s.cleanupSocket()
		st.Close()
		return nil, fmt.Errorf("recover cross-repo workgroup chief-of-staff threads: %w", err)
	}
	if err = s.recoverSessionOpen(ctx); err != nil {
		s.cleanupSocket()
		st.Close()
		return nil, fmt.Errorf("recover an interrupted session open: %w", err)
	}
	s.lifetime, s.cancel = context.WithCancel(ctx)
	if opts.boundary != nil {
		s.boundary = func(step string) error { return opts.boundary(s.lifetime, step) }
	}
	hostname := cfg.Listen.Tailnet
	s.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		defer s.requests.Done()
		sw := &statusResponseWriter{ResponseWriter: w}
		started := time.Now()
		kind := tailnet.ArrivedOn(r)
		defer func() {
			ghlog.Logger(r.Context()).Info("api request handled", "listener", listenerName(kind), "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(started).String())
		}()
		switch kind {
		case tailnet.WebListener:
			if !tailnet.WebAllowed(r) {
				fail(sw, Forbidden)
				return
			}
		case tailnet.TailnetListener:
			if !tailnet.Allowed(r, hostname, s.tailnetNames) {
				fail(sw, Forbidden)
				return
			}
		}
		if !s.ready.Load() {
			fail(sw, Unavailable)
			return
		}
		s.handle(sw, r)
	}), ReadHeaderTimeout: opts.ReadHeaderTimeout, ReadTimeout: opts.ReadTimeout, WriteTimeout: opts.WriteTimeout,
		BaseContext: func(net.Listener) context.Context { return s.lifetime }, ConnContext: tailnet.ConnContext}
	s.ready.Store(true)
	for _, p := range projects {
		s.launch(p)
	}
	s.notifier = newNotifier(s, opts.NotifyRetry, opts.notifyInbox)
	go s.notifier.run(s.lifetime)
	go func() {
		listeners := []net.Listener{listener}
		if s.web != nil {
			listeners = append(listeners, tailnet.Tagged{Listener: s.web, Kind: tailnet.WebListener})
		}
		if s.tailnet != nil {
			go s.superviseTailnet(root, hostname)
		}
		served := make(chan error, len(listeners))
		for _, l := range listeners {
			go func() { served <- s.server.Serve(l) }()
		}
		serving := len(listeners)
		select {
		case <-s.lifetime.Done():
		case e := <-s.failures:
			s.err = e
		case e := <-served:
			if !errors.Is(e, http.ErrServerClosed) {
				s.err = e
			}
			serving--
		}
		s.ready.Store(false)
		// Event streams never go idle, so they end before the drain.
		s.hub.Close()
		drain, stop := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
		if e := s.server.Shutdown(drain); e != nil {
			s.server.Close()
		}
		stop()
		s.cancel()
		<-s.notifier.done
		if s.tailnet != nil {
			<-s.tailnetDone
		}
		for ; serving > 0; serving-- {
			if e := <-served; !errors.Is(e, http.ErrServerClosed) {
				s.err = errors.Join(s.err, e)
			}
		}
		s.requests.Wait()
		s.mu.Lock()
		projects := s.projects
		s.mu.Unlock()
		// A project failure that stopped the service is reported once.
		if e := s.stop(projects...); e != nil && (s.err == nil || !errors.Is(e, s.err)) {
			s.err = errors.Join(s.err, e)
		}
		s.mu.Lock()
		s.projects = nil
		s.mu.Unlock()
		s.cleanupSocket()
		s.store.Close()
		unlock(s.lock)
		reason := "stopped"
		if s.err != nil {
			reason = s.err.Error()
		}
		logger.Info("service stopped", "reason", reason)
		close(s.done)
	}()
	logger.Info("service started", "version", opts.Build.Version, "commit", opts.Build.Commit, "root", root.String(), "listeners", startedListeners(s))
	return s, nil
}

// startedListeners names every listener the service has bound, for the
// service-started log record.
func startedListeners(s *Service) []string {
	listeners := []string{"socket"}
	if s.web != nil {
		listeners = append(listeners, "web")
	}
	if s.tailnet != nil {
		listeners = append(listeners, "tailnet")
	}
	return listeners
}

// listenerName names the listener kind arrivedOn reports, for the
// api-request-handled log record.
func listenerName(kind tailnet.ListenerKind) string {
	switch kind {
	case tailnet.WebListener:
		return "web"
	case tailnet.TailnetListener:
		return "tailnet"
	default:
		return "socket"
	}
}

// statusResponseWriter captures the status code an HTTP handler writes, for
// the api-request-handled log record. Unwrap lets http.ResponseController
// reach the underlying writer's Flush and SetWriteDeadline.
type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Service) Socket() string { return s.cfg.Listen.Socket }

// WebAddr is the bound address of the web listener, or empty when listen.web
// is not configured.
func (s *Service) WebAddr() string {
	if s.web == nil {
		return ""
	}
	return s.web.Addr().String()
}

// TailnetAddr is the address the tailnet listener reports, or empty when
// listen.tailnet is not configured.
func (s *Service) TailnetAddr() string {
	if s.tailnet == nil {
		return ""
	}
	if node := s.tailnet.Current(); node != nil {
		return node.Addr().String()
	}
	return ""
}

// joinTailnet joins the tailnet as hostname with the node's state in the
// root's tailnet directory.
func (s *Service) joinTailnet(root config.Root, hostname string) (TailnetListener, error) {
	dir, err := root.Tailnet()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	join := s.options.JoinTailnet
	if join == nil {
		join = tailnet.JoinTailscale(s.logger)
	}
	return join(hostname, dir)
}

func (s *Service) Wait() error { <-s.done; return s.err }

func (s *Service) Close() error { s.cancel(); return s.Wait() }

// current returns the loaded configuration; project registration replaces it.
func (s *Service) current() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// about returns the project's configuration. A draining project keeps the
// configuration its in-flight work started with.
func (s *Service) about(repository *trace.Repository) *config.Config {
	cfg, projects := s.runtimes()
	if cfg.Active(repository.Project()) {
		return cfg.For(repository.Project())
	}
	for _, p := range projects {
		if p.repository == repository && p.pipeline != nil && p.pipeline.current.Load() != nil {
			return p.pipeline.current.Load().cfg
		}
	}
	return cfg.For(repository.Project())
}

// runtimes returns the loaded configuration and the runtime of every active
// project that has a trace, in active_projects order.
func (s *Service) runtimes() (*config.Config, []*activeProject) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, slices.Clone(s.projects)
}

// runtimeOf returns the loaded configuration about project id and the
// project's runtime, which is nil when the project has no trace. The
// configuration has no project when id is not active.
func (s *Service) runtimeOf(id config.ProjectID) (*config.Config, *activeProject) {
	cfg, projects := s.runtimes()
	cfg = cfg.For(id)
	for _, p := range projects {
		if p.id == id {
			return cfg, p
		}
	}
	return cfg, nil
}

// projectFor resolves the project a project-scoped request names. A request
// may leave the project out only while exactly one project is active; the
// errors list the active projects.
func (s *Service) projectFor(id config.ProjectID) (config.ProjectID, *APIError) {
	ids := s.current().ProjectIDs()
	if id == "" {
		switch len(ids) {
		case 0:
			return "", &APIError{Code: NoProject, Message: "no project is configured; add one with osmia project add"}
		case 1:
			return ids[0], nil
		}
		return "", &APIError{Code: Validation, Message: "several projects are active; name one of " + projectList(ids)}
	}
	if err := config.CheckProjectIDs(id); err != nil {
		return "", &APIError{Code: Validation, Message: "project must be a project ID: p_ followed by 32 lowercase hexadecimal digits"}
	}
	if !slices.Contains(ids, id) {
		if len(ids) == 0 {
			return "", &APIError{Code: NotFound, Message: fmt.Sprintf("project %s is not an active project; no project is active", id)}
		}
		return "", &APIError{Code: NotFound, Message: fmt.Sprintf("project %s is not an active project; the active projects are %s", id, projectList(ids))}
	}
	return id, nil
}

// projectList joins project IDs with commas.
func projectList(ids []config.ProjectID) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return strings.Join(out, ", ")
}

// Run serves in the foreground until cancellation, including full cleanup.
func Run(ctx context.Context, opts Options) error {
	s, err := Start(ctx, opts)
	if err != nil {
		return err
	}
	return s.Wait()
}

// RunSignals adds SIGINT and SIGTERM cancellation for a foreground entry point.
func RunSignals(ctx context.Context, opts Options) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, opts)
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("socket path is not a Unix socket")
	}
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		return fmt.Errorf("socket has a live listener; stop its owner before starting Osmia")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("cannot prove socket is stale: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return fmt.Errorf("socket changed during stale check")
	}
	return os.Remove(path)
}

func (s *Service) cleanupSocket() {
	s.listener.Close()
	if s.web != nil {
		s.web.Close()
	}
	if s.tailnet != nil {
		if node := s.tailnet.Current(); node != nil {
			node.Close()
			node.Leave()
		}
	}
	if info, err := os.Lstat(s.cfg.Listen.Socket); err == nil && os.SameFile(info, s.socketInfo) {
		os.Remove(s.cfg.Listen.Socket)
	}
}

// openAll opens the runtime of every active project of cfg that has a trace,
// in active_projects order, after moving every cross-repo workgroup's Git
// trace to its database. A project whose trace migration could not complete
// is logged and left unopened, so it is not scheduled this run; every other
// project still opens. Any other failure opens none.
func (s *Service) openAll(cfg *config.Config) ([]*activeProject, error) {
	if err := trace.MigrateWorkgroups(context.Background(), cfg.Root, s.migrationLog); err != nil {
		s.log().Warn("workgroup trace migration incomplete", "error", err.Error())
	}
	var out []*activeProject
	for _, id := range cfg.ProjectIDs() {
		p, err := s.open(cfg.For(id))
		if err != nil {
			if errors.Is(err, trace.ErrMigrationIncomplete) {
				s.log().Warn("project trace migration incomplete; project not opened", "project", string(id), "error", err.Error())
				continue
			}
			for _, opened := range out {
				opened.repository.Close()
			}
			return nil, err
		}
		if p != nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// open opens the existing trace of the project cfg is about and its
// reconciliation controller, and joins the project to the shared pool of role
// capacity. Without a configured project, or without a trace for it, the
// project runs idle: trace creation belongs to project registration. A caller
// that closes the project without launching it leaves the pool.
func (s *Service) open(cfg *config.Config) (*activeProject, error) {
	if !cfg.HasProject() {
		return nil, nil
	}
	repository, controller, p, err := s.openReconciliation(cfg)
	if err != nil || repository == nil {
		return nil, err
	}
	if err := ensureChiefsOfStaff(context.Background(), repository, time.Now().UTC()); err != nil {
		repository.Close()
		return nil, err
	}
	if err := cancelAbandoned(context.Background(), repository, s.now); err != nil {
		repository.Close()
		return nil, err
	}
	if err := s.pool.Join(repository); err != nil {
		repository.Close()
		return nil, err
	}
	return &activeProject{id: cfg.Project.ID, repository: repository, controller: controller, conversations: &conversationPath{s: s, repository: repository, pipeline: p, drain: make(chan struct{})}, pipeline: p, done: make(chan error, 1)}, nil
}

// dispatcher is the scheduler of the project cfg is about, drawing on the
// shared pool of role capacity.
func (s *Service) dispatcher(cfg *config.Config, repository *trace.Repository) (*scheduler.Scheduler, error) {
	limits := cfg.Capacity
	limits.PerWorkstream = cfg.Project.Capacity.PerWorkstream
	return scheduler.New(repository, scheduler.Options{Now: s.options.Reconciliation.Now, Holds: s.dispatchHolds(cfg, repository), Admit: s.admit(cfg, repository), Capacity: &limits, Priorities: s.priorities, Shared: s.pool})
}

// dispatchHolds is the scheduler's gate before capacity for the project cfg
// is about: holds of its candidates.
func (s *Service) dispatchHolds(cfg *config.Config, repository *trace.Repository) func(scheduler.Candidate) (bool, error) {
	project := cfg.Project.ID
	librarian := librarianWorkstream(project)
	return func(c scheduler.Candidate) (bool, error) {
		if conversationRole(c.Thread.Identity.Role) {
			return true, nil
		}
		return s.holds(project, librarian, repository, c)
	}
}

// admit is the scheduler's gate after holds and capacity. It holds every
// mason turn of a unit whose workspace is behind its feature branch, so the
// workspace has no writer when the foreman rebases it.
func (s *Service) admit(cfg *config.Config, repository *trace.Repository) func(context.Context, scheduler.Candidate) (bool, error) {
	units := newUnitWorkspaces(cfg, repository)
	return func(ctx context.Context, c scheduler.Candidate) (bool, error) {
		if c.Thread.Identity.Role == masonRole && c.Turn.Request.Unit != "" {
			behind, err := units.behind(ctx, c.Workstream, c.Turn.Request.Unit)
			return !behind, err
		}
		return true, nil
	}
}

// holds reports whether the dispatch gate declines the candidate whatever
// the capacity: a turn of the librarian's workstream, an architect or
// committee turn, which their own passes run, a turn of an abandoned
// workstream, and a turn a pause in force holds. The store is read on every
// pass, so a cleared pause lets held turns run on the loop's next periodic
// pass.
func (s *Service) holds(project config.ProjectID, librarian config.WorkstreamID, repository *trace.Repository, c scheduler.Candidate) (bool, error) {
	if c.Thread.Identity.Role != trace.ChiefOfStaff {
		if held, err := changeHeld(repository, c.Workstream); err != nil || held {
			return true, err
		}
	}
	if c.Workstream == librarian || c.Thread.Identity.Role == architectRole || c.Thread.Identity.Role == committeeRole {
		return true, nil
	}
	if gone, err := abandoned(repository, c.Workstream); err != nil || gone {
		return gone, err
	}
	if (c.Thread.Identity.Role == masonRole || c.Thread.Identity.Role == reviewerRole) && c.Turn.Request.Unit != "" {
		approved, err := repository.GroupApproved(c.Workstream)
		if err != nil || !approved {
			return true, err
		}
		if held, err := assignmentHeld(repository, c.Workstream, c.Turn.Request.Unit, c.Thread.Identity.Role); err != nil || held {
			return held, err
		}
	}
	if c.Thread.Identity.Role != trace.ChiefOfStaff {
		if waiting, err := baseWaiting(repository, c.Workstream); err != nil || waiting {
			return waiting, err
		}
		approved, err := repository.GroupApproved(c.Workstream)
		if err != nil {
			return true, err
		}
		// Information-only role turns may ask during drafting; unit work remains
		// held by the group approval and edge admission checks.
		if approved || c.Turn.Request.Unit != "" {
			if waiting, err := edgeWaiting(repository, c.Workstream); err != nil || waiting {
				return waiting, err
			}
		}
	}
	st, _ := s.effective()
	return scheduler.Held(st.Pauses, project, c), nil
}

// priorities returns the runtime priority order in force.
func (s *Service) priorities() []runtime.Priority {
	st, _ := s.effective()
	return st.Priorities
}

// chiefEventsProfile returns the profile of a new chief-of-staff turn that
// delivers service events: the owner's override of the role's profile when
// there is one, else roles.chief_of_staff.events_profile when it is set, else
// the role's profile.
func (s *Service) chiefEventsProfile(cfg *config.Config) func() (coreadapter.Profile, error) {
	return func() (coreadapter.Profile, error) {
		st, _ := s.effective()
		if name := cfg.Roles[trace.ChiefOfStaff].EventsProfile; name != "" && st.Profiles[trace.ChiefOfStaff] == "" {
			return cfg.NamedProfile(name)
		}
		_, profile, err := s.chiefOverride(cfg)
		return profile, err
	}
}

// chiefOverride resolves the chief of staff's effective profile name, which
// may be any configured profile, not only one in the role's fallback chain.
func (s *Service) chiefOverride(cfg *config.Config) (string, coreadapter.Profile, error) {
	st, _ := s.effective()
	name := st.Profiles[trace.ChiefOfStaff]
	if name == "" {
		name = cfg.Roles[trace.ChiefOfStaff].Profile
	}
	profile, err := cfg.NamedProfile(name)
	return name, profile, err
}

// ensureChiefsOfStaff gives every workstream in the trace its chief-of-staff
// thread, leaving existing threads untouched. A session never gets one: it
// has no chief of staff.
func ensureChiefsOfStaff(ctx context.Context, repository *trace.Repository, at time.Time) error {
	streams, err := repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if session, err := isSession(repository, stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		} else if session {
			continue
		}
		if id, err := repository.CrossRepoWorkgroupOf(stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		} else if id != "" {
			// A cross-repo member's shared chief-of-staff thread lives
			// only in its workgroup's own trace (chiefRepository), lazily
			// created there the first time it is needed; this
			// project-scoped backfill must never create a second one on
			// the member's own repo, which would make the workgroup's
			// chief of staff authoritative in two places (charter#2).
			continue
		}
		if pending, err := repository.PendingCrossRepoWorkgroup(stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		} else if pending != "" {
			continue
		}
		if _, err := repository.EnsureChiefOfStaff(ctx, stream, at, serviceActor); err != nil {
			return fmt.Errorf("workstream %s chief-of-staff thread: %w", stream, err)
		}
	}
	return nil
}

// launch runs the controller for the service's lifetime. A loop failure stops
// the service; cancellation from stop or shutdown does not.
func (s *Service) launch(active *activeProject) {
	if active == nil {
		return
	}
	s.log().Info("project started", "project", string(active.id))
	ctx, cancel := context.WithCancel(s.lifetime)
	active.cancel = cancel
	pollCtx, stopPolls := context.WithCancel(ctx)
	active.pollCancel = stopPolls
	go func() {
		watchDone := make(chan error, 1)
		go func() {
			err := s.watchMemory(ctx, active.repository)
			if err != nil {
				cancel()
			}
			watchDone <- err
		}()
		conversationDone := make(chan error, 1)
		go func() {
			err := active.conversations.run(ctx)
			if err != nil && ctx.Err() == nil {
				cancel()
			}
			conversationDone <- err
		}()
		pollDone := make(chan struct{})
		go func() {
			s.runNetworkPolls(pollCtx, active.repository, active.pipeline)
			close(pollDone)
		}()
		err := active.controller.Run(ctx)
		stopPolls()
		<-pollDone
		active.conversations.Drain()
		if err != nil {
			cancel()
		}
		if conversationErr := <-conversationDone; conversationErr != nil && !errors.Is(conversationErr, context.Canceled) {
			err = conversationErr
		}
		cancel()
		if watchErr := <-watchDone; watchErr != nil {
			err = watchErr
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			err = fmt.Errorf("project %s: %w", active.id, err)
		}
		active.done <- err
		close(active.done)
		if err != nil && !errors.Is(err, context.Canceled) {
			select {
			case s.failures <- err:
			default:
			}
		}
	}()
}

// halt cancels the project's controller and joins it. It reports loop
// failures other than cancellation.
func (s *Service) halt(active *activeProject) error {
	if active.cancel == nil {
		return nil
	}
	active.cancel()
	if e := <-active.done; e != nil && !errors.Is(e, context.Canceled) {
		return e
	}
	return nil
}

// stop joins every project's controller, then releases their traces, so no
// scheduler pass reads a trace that is closing. It reports loop failures
// other than cancellation.
func (s *Service) stop(projects ...*activeProject) error {
	for _, p := range projects {
		if p.cancel != nil {
			p.cancel()
		}
	}
	var err error
	for _, p := range projects {
		haltErr := s.halt(p)
		err = errors.Join(err, haltErr)
		if haltErr != nil {
			s.log().Error("project stopped", "project", string(p.id), "reason", "service stopped", "error", haltErr.Error())
		} else {
			s.log().Info("project stopped", "project", string(p.id), "reason", "service stopped")
		}
	}
	for _, p := range projects {
		err = errors.Join(err, p.repository.Close())
	}
	return err
}

// migrationLog reports trace.Migrate's progress through s's structured
// logger, naming only the project and workstream, never transcript content
// or credentials, the way the tailnet supervisor reports its own retries.
func (s *Service) migrationLog(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if strings.Contains(format, "failed") {
		s.log().Warn(msg)
		return
	}
	s.log().Info(msg)
}

// openReconciliation leaves trace creation to project registration; an existing
// trace must open cleanly before the service can report readiness. The runner
// boundary is served by the bound thread reconciler for turns and by the
// service's own reconcilers for knowledge-base extraction and refresh, architect drafts,
// committee rounds and the architect's replies to them, and final reviews, and the repository
// boundary by the service's sealer for sealings, its builder for builds and
// its foreman for landings and rebases and its publisher for publications;
// the daily budget, then the loop guard, then the architect controller, then the shed controller, then the sealing
// controller, then the building controller, then the overlap, charter, refresh, drift,
// landing, assembly and publication controllers run at the start of every pass, and the pass reconciles operations in stagePriority order. With
// Options.Threads, outbox events are then delivered to each workstream's
// chief of staff, recorded answers are queued on their askers' threads, the
// mason controller parks, resumes and starts units, and the scheduler admits
// worker turns within capacity and its runtime and workspace gates. Conversation
// queues are reconciled independently by conversationPath;
// without it, the configured Schedule hook runs instead. The hooks and
// adapters come from the pipeline, which a reload may restage for the next pass.
// The loop holds the pending operations of the architect and committee
// reconcilers while a pause covers their workstream, and runs model workflows,
// thread turns and committee rounds beside the pass under their own ordering.
func (s *Service) openReconciliation(cfg *config.Config) (*trace.Repository, *reconcile.Controller, *pipeline, error) {
	options := s.options.Reconciliation
	directory, err := cfg.Root.ProjectTrace(cfg.Project.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	if exists, err := trace.HasTrace(directory); err != nil {
		return nil, nil, nil, err
	} else if !exists {
		return nil, nil, nil, nil
	}
	if err := trace.Migrate(context.Background(), cfg.Root, cfg.Project.ID, plan.DraftCheck(directory), s.migrationLog); err != nil {
		return nil, nil, nil, err
	}
	repository, err := trace.Open(cfg.Root, cfg.Project)
	if err != nil {
		if repository != nil {
			repository.Close()
		}
		return nil, nil, nil, err
	}
	repository.SetCharterResolver(s.charterRepository)
	observe := s.hub.TraceEvents(cfg.Project.ID, librarianWorkstream(cfg.Project.ID))
	repository.Observe(func(c trace.Commit) { observe(c); s.conversationSignal.wake() })
	if options.Worker == "" {
		options.Worker = "local-operations"
	}
	first, err := s.stages(cfg, repository)
	if err != nil {
		repository.Close()
		return nil, nil, nil, err
	}
	p := &pipeline{slowThreshold: options.SlowThreshold, before: s.step}
	p.current.Store(first)
	p.conversation.Store(first)
	options.Schedule = p.schedule
	if hook := s.options.schedulePassed; hook != nil {
		schedule := options.Schedule
		options.Schedule = func(ctx context.Context) error {
			if err := schedule(ctx); err != nil {
				return err
			}
			hook()
			return nil
		}
	}
	adapters := maps.Clone(options.Adapters)
	if adapters == nil {
		adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{}
	}
	adapters[coreadapter.RunnerBoundary] = activityReconciler{stagedAdapter{p, func(st *stages) coreadapter.Reconciler { return st.runner }}, s.activity, cfg.Project.ID}
	adapters[coreadapter.RepositoryBoundary] = activityReconciler{stagedAdapter{p, func(st *stages) coreadapter.Reconciler { return st.repository }}, s.activity, cfg.Project.ID}
	options.Adapters = adapters
	if options.Priority == nil {
		options.Priority = stagePriority
	}
	if options.Hold == nil {
		options.Hold = s.holding(repository)
	}
	hold := options.Hold
	options.Hold = func(stream config.WorkstreamID, op coreadapter.Operation) bool {
		return conversationOperation(op) || hold(stream, op)
	}
	if options.Concurrent == nil {
		options.Concurrent = concurrentOperation
	}
	if options.Class == nil {
		options.Class = operationClass
	}
	options.Limit = s.modelOperationLimit(repository, options.Limit)
	options.Limit = s.gitOperationLimit(repository, options.Limit)
	controller, err := reconcile.New(repository, options)
	if err != nil {
		repository.Close()
		return nil, nil, nil, err
	}
	return repository, controller, p, nil
}

// stages builds the schedule hooks and operation adapters of cfg over
// repository. Building them runs no hook and applies no operation.
func (s *Service) stages(cfg *config.Config, repository *trace.Repository) (*stages, error) {
	options, threads := s.options.Reconciliation, s.options.Threads
	draft := &drafter{S: s.intake().DrafterDependencies(), Repository: repository}
	rounds := &debate{S: s.intake().DebateDependencies(), Repository: repository}
	seals := &sealer{S: s.intake().SealerDependencies(), Repository: repository}
	build := &builder{S: s.intake().BuilderDependencies(), Repository: repository}
	overlap := &overlaps{s: s, repository: repository}
	rules := charterer{S: s.intake().ChartererDependencies(), Repository: repository}
	land := &foreman{masons: &masons{s: s, cfg: cfg, repository: repository}}
	refresh := &refresher{extractor: &extractor{s: s, repository: repository}}
	finals := &delivery.FinalReviewer{S: s.delivery(), Repository: repository}
	publish := &delivery.Publisher{S: s.delivery(), Repository: repository}
	amend := &amendmentDrafter{Drafter: draft}
	amendRounds := amendmentDebate{Debate: rounds}
	budget := budgetSignals{s: s, repository: repository}
	daily := dailyBudget{s: s, repository: repository}
	checks := &checkers{masons: &masons{s: s, cfg: cfg, repository: repository}}
	runner := runnerAdapter{deltas: deltaRunner{S: s.lifecycle()}, turns: options.Adapters[coreadapter.RunnerBoundary], extract: refresh.extractor, refresh: refresh, draft: draft, amend: amend, amendRounds: amendRounds, rounds: rounds, finals: finals, checks: checks}
	cleanup := &workspaceCleanup{Cfg: cfg, Repository: repository, Now: s.now}
	hooks := []scheduleHook{{"workgroup-change", func(ctx context.Context) error { return s.changePass(ctx, repository) }}, {"base-refresh", (&baseRefresher{S: s.lifecycle(), Repository: repository}).Pass}, {"base", func(ctx context.Context) error { return s.baseWaitPass(ctx, repository) }}, {"edge-refresh", func(ctx context.Context) error { return s.edgeResolverPass(ctx, repository) }}, {"edge", func(ctx context.Context) error { return s.edgeWaitPass(ctx, repository) }}, {"daily-budget", daily.Pass}, {"loop-guard", loopGuard{s: s, cfg: cfg, repository: repository}.Pass}, {"design-start", func(ctx context.Context) error { return s.designStartPass(ctx, repository) }}, {"draft", draft.Pass}, {"budget", budget.Pass}, {"amendment", amend.Pass}, {"amendment-debate", amendRounds.Pass}, {"debate", rounds.Pass}, {"group-approval", func(ctx context.Context) error { return s.groupApprovalPass(ctx, repository) }}, {"seal", seals.Pass}, {"build", build.Pass}, {"overlap", overlap.Pass}, {"charter", rules.Pass}, {"refresh", refresh.Pass}, {"drift", land.Drifts}, {"land", land.Pass}, {"final-review", finals.Pass}, {"publish", publish.Pass}, {"cleanup", cleanup.Pass}, {"archive-cleanup", (&archiveCleanup{S: s.lifecycle(), Workspaces: cleanup}).Pass}, {"workgroup-resolution", (&workgroupResolver{S: s.lifecycle(), Repository: repository}).Pass}, {"workgroup-project-spec", func(ctx context.Context) error { return s.projectSpecDraftPass(ctx, repository) }}, {"workgroup-project-spec-refresh", func(ctx context.Context) error { return s.projectSpecRefreshPass(ctx, repository) }}}
	hooks = append([]scheduleHook{{"factory-policy", func(ctx context.Context) error { return s.reconcileFactoryPolicy(ctx, repository) }}}, hooks...)
	if threads == nil && options.Schedule != nil {
		hooks = append(hooks, scheduleHook{"configured", options.Schedule})
	}
	if threads != nil {
		bound, err := threads(repository, cfg)
		if err != nil {
			return nil, err
		}
		runner.turns = abandonable{Reconciler: bound, s: s, repository: repository, cfg: cfg}
		dispatch, err := s.dispatcher(cfg, repository)
		if err != nil {
			return nil, err
		}
		deliver, err := events.New(repository, events.Options{Now: options.Now, Window: cfg.EventWindow(), ProgressWindow: cfg.EventProgressWindow(), Profile: s.chiefEventsProfile(cfg), System: s.chiefEventsPrompt(cfg.Project.ID, repository),
			Skip: s.chiefEventsSkip(repository), ChiefTarget: func(ctx context.Context, stream config.WorkstreamID) (*trace.Repository, func(), error) {
				target, _, _, close, err := s.chiefRepository(ctx, repository, stream)
				if errors.Is(err, errWorkgroupJoining) {
					return nil, close, fmt.Errorf("%w: %v", events.ErrChiefUnavailable, err)
				}
				return target, close, err
			}})
		if err != nil {
			return nil, err
		}
		units := &masons{s: s, cfg: cfg, repository: repository}
		reviews := &reviewers{masons: units}
		hooks = append(hooks, scheduleHook{"events", deliver.Pass}, scheduleHook{"answers", s.answers(cfg, repository).Pass}, scheduleHook{"checks", checks.Pass}, scheduleHook{"reviews", reviews.Pass}, scheduleHook{"masons", units.Pass}, scheduleHook{"dispatch", dispatch.Pass})
	}
	var polls, schedule []scheduleHook
	for _, hook := range hooks {
		switch hook.name {
		case "base-refresh", "edge-refresh", "workgroup-change", "drift":
			polls = append(polls, hook)
		default:
			schedule = append(schedule, hook)
		}
	}
	return &stages{cfg: cfg, hooks: schedule, polls: polls, runner: runner,
		repository: repositoryAdapter{other: options.Adapters[coreadapter.RepositoryBoundary], seals: seals, builds: build, lands: land, publishes: publish}}, nil
}

// concurrentOperation selects operations whose inspection and effects run
// beside the scheduling pass under their operation class's ordering.
func concurrentOperation(op coreadapter.Operation) bool {
	if gitOperation(op) || networkOperation(op) || modelOperationRole(op) != "" {
		return true
	}
	if op.Boundary != coreadapter.RunnerBoundary {
		return false
	}
	switch op.Action {
	case thread.TurnAction, RoundAction, AmendmentRoundAction, CheckAction, FinalReviewAction, deliveryReviewAction, deltaTurnAction:
		return true
	}
	return false
}

// operationClass keeps execution order independent of which operations run
// beside the pass. Scheduler-admitted workers share no workflow reservation;
// owner conversations rely on their durable thread order.
func operationClass(op coreadapter.Operation) trace.OperationClass {
	if networkOperation(op) {
		return trace.WorkerOperation
	}
	if op.Boundary == coreadapter.RunnerBoundary {
		switch op.Action {
		case thread.TurnAction:
			in, err := thread.DecodeTurn(op)
			if err == nil && (in.Agent == trace.ChiefOfStaff || in.Agent == sessionAgentID) {
				return trace.ConversationOperation
			}
			return trace.WorkerOperation
		case RoundAction, AmendmentRoundAction, CheckAction, deltaTurnAction:
			return trace.WorkerOperation
		}
	}
	return trace.WorkflowOperation
}

// stagePriority orders the operations of a pass so that the factory finishes
// work before it widens it: the turns the scheduler dispatched and everything
// else first, then the shed's rounds, replies and redrafts, then architect
// drafts.
func stagePriority(op coreadapter.Operation) int {
	switch op.Action {
	case RoundAction, ReplyAction, RedraftAction, AmendmentRoundAction, AmendmentReplyAction:
		return 1
	case DraftAction, AmendmentDraftAction:
		return 2
	}
	return 0
}

// unlock releases the root ownership lock before closing its file. A child
// process forked but not yet executed shares the open file, and closing alone
// would leave the lock held until that child executes.
func unlock(lock *os.File) {
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	lock.Close()
}
