// Package service runs the Osmia factory: it owns each active project's trace
// and runtime state, drives the reconciliation, scheduling and conversation
// loops, and serves the local HTTP API listed in Routes together with the Go
// client for it.
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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kpenfound/agentcore/agent"
	corejev "github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/clientturns"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/eventstream"
	"github.com/kpenfound/osmia/internal/filetransport"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/skills"
	"github.com/kpenfound/osmia/internal/slotspans"
	"github.com/kpenfound/osmia/internal/tailnet"
	"github.com/kpenfound/osmia/internal/telemetry"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Options configures Start. Zero values select the defaults documented on
// each field.
type Options struct {
	reviewChecks ReviewChecks
	cloudChecks  ReviewChecks
	boundary     func(context.Context, string) error
	Config       config.Options
	Build        Identity
	// Workstreams supplies known identities when no trace exists. An existing
	// trace supplies its validated workstream manifests.
	Workstreams       []config.WorkstreamID
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	Reconciliation    reconcile.Options
	// Threads builds the runner-boundary reconciler for a project's trace and
	// the configuration the service has loaded. The service calls it each time
	// a project's trace opens. With Threads set the service dispatches queued
	// workstream turns on its own, one turn per thread at a time and within
	// the configured capacity, in place of Reconciliation.Schedule. Callers
	// must not close the repository.
	Threads func(*trace.Repository, *config.Config) (coreadapter.Reconciler, error)
	// Architect runs the architect's drafting, reply and redraft turns.
	// Without it none is requested: requested ones stay pending, and handed
	// workstreams, heard rounds and requested redrafts wait.
	Architect *Architect
	// Committee runs the committee's shed rounds and final reviews. Without it
	// none is requested and requested ones stay pending; a workstream whose
	// debate the owner skipped at hand-in still enters the shed.
	Committee *Committee
	// Issues fetches issue URLs handed in. It defaults to the GitHub REST API
	// with the service's GITHUB_TOKEN environment variable, which no session
	// receives.
	Issues issues.Client
	// PullRequests finds and opens the pull requests that deliver approved
	// workstreams. It defaults to the GitHub REST API with the service's
	// GITHUB_TOKEN environment variable, which no session receives.
	PullRequests pulls.Client
	// CloudStatuses reads the combined commit statuses of a pushed commit,
	// which the final review watches. It defaults to PullRequests when that
	// reads statuses, as the GitHub client does.
	CloudStatuses pulls.StatusReader
	// CloudRerun requests the rerun of a pull request's failed Cloud checks.
	// It defaults to the Dagger CLI with the API token
	// dagger.cloud_api_token_env names.
	CloudRerun CloudRerunner
	// CloudTrace fetches the output of a Dagger Cloud trace. It defaults to
	// the Dagger CLI with the API token dagger.cloud_api_token_env names.
	CloudTrace TraceRunner
	// CloudTraceTimeout replaces the time one automatic trace fetch of a
	// failed check may hold the final review. Zero keeps the default.
	CloudTraceTimeout time.Duration
	// FinalChecksWait replaces the 2 minutes after a push during which the
	// final review reads no status. Nil keeps them.
	FinalChecksWait *time.Duration
	// Repositories reads and creates the GitHub repositories of project
	// creation. It defaults to the GitHub REST API with the service's
	// GITHUB_TOKEN environment variable, which no session receives.
	Repositories pulls.Repositories
	// JevProvider returns the Jev provider for the current settings and API
	// key. It defaults to the TypeSafe-compatible HTTP API the settings name.
	JevProvider func(corejev.Settings, string) corejev.Provider
	// JoinTailnet joins the owner's tailnet when listen.tailnet is set. It
	// defaults to embedded Tailscale.
	JoinTailnet tailnet.Join
	// TailnetRetry is how long the service waits before it joins the tailnet
	// again after a failed join or a dropped listener. It defaults to 5
	// seconds.
	TailnetRetry time.Duration
	// TailnetHTTPSRetry is how long the service waits before it requests the
	// tailnet HTTPS certificate again after a failed request, doubled after
	// each further failure up to 15 minutes. It defaults to 30 seconds.
	TailnetHTTPSRetry time.Duration
	// ClientIdentity names the tailnet node a client protocol request came
	// from. It defaults to the WhoIs of the service's own tailnet node.
	ClientIdentity func(*http.Request) (fleet.Identity, error)
	// Fleet tunes the client leases: the clock, the grace window and the
	// heartbeat and poll intervals. Its zero value selects the defaults.
	Fleet fleet.Options
	// NotifyRetry is how long the service waits before it posts a
	// Notification the webhook refused again, doubled after each further
	// failure. It defaults to 30 seconds.
	NotifyRetry time.Duration
	// ErrorReporter tunes the error reporter, which runs when [error_reports]
	// enables it. Its zero value selects the defaults.
	ErrorReporter ErrorReporterOptions
	// Location is the service host's time zone, whose calendar days the daily
	// budget counts. It defaults to the host's local time zone.
	Location *time.Location
	// Logger is the structured logger every line the service writes to its
	// standard output and standard error goes through: the service's own
	// records, embedded Tailscale's and the GitHub activity log. It defaults
	// to a JSON handler on os.Stderr.
	Logger            *slog.Logger
	controls          *runtimeControls
	skills            *skills.Manager
	notifyInbox       func(context.Context) (InboxResponse, *APIError)
	checkJJ           jjCheck
	conversationTicks <-chan time.Time
	schedulePassed    func()
	hearsayPassed     func()
	gateScanned       func()
	getenv            func(string) string
	telemetryExporter sdktrace.SpanExporter
	transcribeLimit   int64
	transcribeTimeout time.Duration
}

type activeProject struct {
	id            config.ProjectID
	repository    *trace.Repository
	controller    *reconcile.Controller
	conversations *conversationPath
	pipeline      *pipeline
	cancel        context.CancelFunc
	pollCancel    context.CancelFunc
	done          chan error
	gates         *gateWorker
	climb         *climbWorker
}

// Service is a running factory service. Start returns it once it is bound and
// ready; Wait joins its shutdown.
type Service struct {
	conversationSignal    conversationSignal
	memory                hearsay.Health
	jev                   *jev.Judge
	router                *router.Router
	routing               *router.Worker
	stalls                stallClocks
	gateWorkers           sync.Map
	climbWorkers          sync.Map
	reseeders             sync.Map
	mu                    sync.Mutex
	cfg                   *config.Config
	revisions             map[config.ProjectID]int
	projects              []*activeProject
	telemetry             *telemetry.Sink
	slots                 *slotspans.Recorder
	capacity              atomic.Pointer[config.Capacity]
	spans                 *factorySpans
	pool                  *scheduler.Shared
	gitOperations         gitOperationPool
	checkSlots            *checkSlotPool
	chief                 *chiefSlotPool
	committeeAdmissions   sync.Map
	modelOperationStreams sync.Map
	spends                daySpends
	pending               error
	reloadErr             *ReloadError
	projectMu             sync.Mutex
	handInMu              sync.Mutex
	flakeFixMu            sync.Mutex
	sessionMu             sync.Mutex
	turns                 runningTurns
	options               Options
	store                 *runtime.Store
	logger                *slog.Logger
	lock                  *os.File
	listener              *net.UnixListener
	web                   net.Listener
	tailnet               *tailnet.Link
	tailnetDone           chan struct{}
	transfers             *filetransport.Registry
	fleet                 *fleet.Registry
	clients               *clientturns.Dispatcher
	placement             *fleet.Placement
	socketInfo            os.FileInfo
	server                *http.Server
	lifetime              context.Context
	cancel                context.CancelFunc
	failures              chan error
	done                  chan struct{}
	err                   error
	ready                 atomic.Bool
	requests              sync.WaitGroup
	boundaryMu            sync.RWMutex
	boundary              func(string) error
	hub                   *eventstream.Hub
	activity              *activity.Registry
	notifier              *notifier
	reporter              *errorReporter
	driftAsked            atomic.Bool
	polls                 lifecycle.PullRequestAttempts
	statusViews           readview.Cache[StatusResponse]
	removed               removedPublication
	removedDone           chan struct{}
}

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
	capture := newErrorCapture(opts.ErrorReporter.Queue, opts.ErrorReporter.Now)
	logger = slog.New(capture.wrap(logger.Handler()))
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
	if opts.Repositories == nil {
		opts.Repositories = pulls.GitHub{Token: os.Getenv("GITHUB_TOKEN"), HTTP: &http.Client{Timeout: 30 * time.Second}}
	}
	cfg, err := projectstore.LoadConfig(opts.Config)
	if err != nil {
		return nil, err
	}
	registerHearsaySecrets(cfg.Hearsay)
	transfers, err := root.Transfers()
	if err != nil {
		return nil, err
	}
	if opts.Fleet.Now == nil {
		opts.Fleet.Now = func() time.Time {
			if opts.Reconciliation.Now != nil {
				return opts.Reconciliation.Now().UTC()
			}
			return time.Now().UTC()
		}
	}
	s := &Service{transfers: filetransport.NewRegistry(transfers), fleet: fleet.NewRegistry(opts.Fleet), options: opts, lock: lock, failures: make(chan error, 1), done: make(chan struct{}), hub: eventstream.New(), logger: logger}
	s.telemetry = s.startTelemetry(cfg.Telemetry)
	s.slots = s.startSlotSpans(cfg)
	s.spans = newFactorySpans(s.telemetry, ledgerPath(root), s.now, logger)
	defer func() {
		if err != nil {
			s.slots.Close()
			s.telemetry.Close()
		}
	}()
	if err := s.ensureEngine(ctx, cfg); err != nil {
		return nil, err
	}
	s.activity = activity.New(s.now)
	s.activity.Notify = func() { s.hub.Publish(Event{Kind: api.EventActivity}) }
	s.clients = clientturns.New(clientturns.Dispatcher{Fleet: s.fleet, Transfers: s.transfers, Activity: s.activity, Directory: transfers, Views: filepath.Join(root.String(), "views"), Now: s.now})
	s.placement = &fleet.Placement{Registry: s.fleet, LocalSlots: s.localSlots}
	s.clients.SetChooser(func(_ context.Context, scope coreadapter.Scope, _ coreadapter.Profile) (fleet.Identity, bool) {
		return s.placement.Locate(scope.Turn)
	})
	s.pool = &scheduler.Shared{Changed: func() { s.hub.Publish(Event{Kind: EventCapacity}) }, Spans: s.slots, Limits: s.poolLimit}
	s.checkSlots = &checkSlotPool{limit: cfg.Capacity.Checks, changed: func() { s.hub.Publish(Event{Kind: EventCapacity}) }, spans: s.slots, now: s.now}
	s.chief = newChiefSlotPool(cfg.Capacity.Manager, func() { s.hub.Publish(Event{Kind: EventCapacity}) }, s.conversationSignal.wake)
	s.slots.Interrupt(telemetry.PoolID(checksRole))
	s.jev = &jev.Judge{Config: func() config.Jev { return s.current().Jev }, Provider: opts.JevProvider, Getenv: func(name string) string {
		value := os.Getenv(name)
		redact.Register(value)
		return value
	}}
	s.router = s.newRouter()
	if opts.skills != nil {
		opts.skills.Refresh = func() string { return s.current().Skills.Refresh }
	}
	if opts.controls != nil {
		opts.controls.service.Store(s)
	}
	if opts.boundary != nil {
		s.setBoundary(func(step string) error { return opts.boundary(ctx, step) })
	}
	pendingRecovery := s.StartActivity("recovery-pending", ActivityRecovery, "", "", "recovering an interrupted project registration")
	cfg, s.pending = s.recoverPending(ctx, cfg)
	pendingRecovery.Finish(s.pending)
	revisions, err := effectiveRevisions(cfg)
	if err != nil {
		return nil, err
	}
	s.cfg, s.revisions = cfg, revisions
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
	st.ObservePauseEnd(func(p runtime.Pause, replaced bool) { s.spans.pauseEnded(p, s.now(), replaced) })
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
		s.tailnet, s.tailnetDone = &tailnet.Link{HTTPSEnabled: cfg.Listen.TailnetHTTPS}, make(chan struct{})
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
	if opts.TailnetHTTPSRetry <= 0 {
		opts.TailnetHTTPSRetry = 30 * time.Second
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
	s.lifecycle().RecoverArchiveLifecycle(ctx)
	if s.tailnet != nil {
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
		reapSandboxes(ctx, view, agent.SandboxCLI)
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
	s.routing = router.NewWorker(s.lifetime, s.router)
	if opts.boundary != nil {
		s.setBoundary(func(step string) error { return opts.boundary(s.lifetime, step) })
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
	s.spans.begin(poolsOf(cfg.Capacity))
	s.fleet.SetHandout(fleet.Handout(cfg))
	s.ready.Store(true)
	for _, p := range projects {
		s.launch(p)
	}
	s.notifier = newNotifier(s, opts.NotifyRetry, opts.notifyInbox)
	go s.notifier.run(s.lifetime)
	s.startFleet(s.lifetime)
	s.reporter = newErrorReporter(s, capture)
	s.reporter.configure(cfg)
	go s.reporter.run(s.lifetime)
	s.removedDone = make(chan struct{})
	go func() {
		defer close(s.removedDone)
		s.publishRemoved(s.lifetime)
	}()
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
		s.hub.Close()
		drain, stop := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
		if e := s.server.Shutdown(drain); e != nil {
			s.server.Close()
		}
		stop()
		s.cancel()
		s.routing.Wait()
		<-s.notifier.done
		<-s.reporter.done
		<-s.removedDone
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
		if e := s.stop(projects...); e != nil && (s.err == nil || !errors.Is(e, s.err)) {
			s.err = errors.Join(s.err, e)
		}
		s.mu.Lock()
		s.projects = nil
		s.mu.Unlock()
		s.slots.Close()
		s.spans.end()
		if e := s.telemetry.Close(); e != nil {
			logger.Warn("telemetry spans were not all sent", "component", "telemetry", "error", e.Error())
		}
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

func (s *Service) current() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

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

func (s *Service) runtimes() (*config.Config, []*activeProject) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, slices.Clone(s.projects)
}

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

func projectList(ids []config.ProjectID) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return strings.Join(out, ", ")
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
			s.tailnet.Set(nil, "the service has stopped")
		}
	}
	if info, err := os.Lstat(s.cfg.Listen.Socket); err == nil && os.SameFile(info, s.socketInfo) {
		os.Remove(s.cfg.Listen.Socket)
	}
}

func (s *Service) openAll(cfg *config.Config) ([]*activeProject, error) {
	var out []*activeProject
	for _, id := range cfg.ProjectIDs() {
		p, err := s.open(cfg.For(id))
		if err != nil {
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

func (s *Service) open(cfg *config.Config) (*activeProject, error) {
	if !cfg.HasProject() {
		return nil, nil
	}
	repository, controller, p, err := s.openReconciliation(cfg)
	if err != nil || repository == nil {
		return nil, err
	}
	if err := EnsureChiefsOfStaff(context.Background(), repository, time.Now().UTC()); err != nil {
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
	active := &activeProject{id: cfg.Project.ID, repository: repository, controller: controller, conversations: &conversationPath{s: s, repository: repository, pipeline: p, drain: make(chan struct{})}, pipeline: p, done: make(chan error, 1)}
	return active, nil
}

func (s *Service) dispatcher(cfg *config.Config, repository *trace.Repository) (*scheduler.Scheduler, error) {
	limits := cfg.Capacity
	limits.PerWorkstream = cfg.Project.Capacity.PerWorkstream
	return scheduler.New(repository, scheduler.Options{Now: s.options.Reconciliation.Now, Holds: s.dispatchHolds(cfg, repository), Admit: s.admit(cfg, repository), Capacity: &limits, Priorities: s.priorities, Shared: s.pool, Placement: s.placement, Resumable: resumableRole})
}

func (s *Service) localSlots() (int, bool) {
	cfg := s.current()
	if cfg == nil || cfg.Capacity.LocalSlots == nil {
		return 0, false
	}
	return *cfg.Capacity.LocalSlots, true
}

func resumableRole(role string) bool { return role == sessionRole }

func (s *Service) dispatchHolds(cfg *config.Config, repository *trace.Repository) func(scheduler.Candidate) (bool, error) {
	project := cfg.Project.ID
	return func(c scheduler.Candidate) (bool, error) {
		if conversationRole(c.Thread.Identity.Role) {
			return true, nil
		}
		return s.holds(project, repository, c)
	}
}

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

func (s *Service) holds(project config.ProjectID, repository *trace.Repository, c scheduler.Candidate) (bool, error) {
	if !trace.ManagerLike(c.Thread.Identity.Role) {
		if held, err := changeHeld(repository, c.Workstream); err != nil || held {
			return true, err
		}
	}
	if c.Thread.Identity.Role == architectRole || c.Thread.Identity.Role == committeeRole {
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
	if !trace.ManagerLike(c.Thread.Identity.Role) {
		if waiting, err := baseWaiting(repository, c.Workstream); err != nil || waiting {
			return waiting, err
		}
		approved, err := repository.GroupApproved(c.Workstream)
		if err != nil {
			return true, err
		}
		if approved || c.Turn.Request.Unit != "" {
			if waiting, err := edgeWaiting(repository, c.Workstream); err != nil || waiting {
				return waiting, err
			}
		}
	}
	st, _ := s.effective()
	return scheduler.Held(st.Pauses, project, c), nil
}

func (s *Service) priorities() []runtime.Priority {
	st, _ := s.effective()
	return st.Priorities
}

func (s *Service) chiefEventsProfile(cfg *config.Config) func() (coreadapter.Profile, error) {
	return func() (coreadapter.Profile, error) {
		st, _ := s.effective()
		if name := cfg.Roles[config.ManagerRole].EventsProfile; name != "" && st.Profiles[trace.Manager] == "" {
			return cfg.NamedProfile(name)
		}
		_, profile, err := s.chiefOverride(cfg)
		return profile, err
	}
}

func (s *Service) chiefOverride(cfg *config.Config) (string, coreadapter.Profile, error) {
	st, _ := s.effective()
	name := st.Profiles[trace.Manager]
	if name == "" {
		name = cfg.Roles[config.ManagerRole].Profile
	}
	profile, err := cfg.NamedProfile(name)
	return name, profile, err
}

// designerOverride reads the profile design turns run on. The designer is a
// role of its own: it never takes the manager's profile or its
// override, only its own configured profile, which is the default profile
// when the configuration names none.
func (s *Service) designerOverride(cfg *config.Config) (string, coreadapter.Profile, error) {
	st, _ := s.effective()
	name := st.Profiles[trace.Designer]
	if name == "" {
		name = cfg.Roles[trace.Designer].Profile
	}
	profile, err := cfg.NamedProfile(name)
	return name, profile, err
}

func EnsureChiefsOfStaff(ctx context.Context, repository *trace.Repository, at time.Time) error {
	streams, err := repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if session, err := intake.IsSession(repository, stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		} else if session {
			continue
		}
		if design, err := intake.IsDesignWorkstream(repository, stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		} else if design {
			continue
		}
		if id, err := repository.CrossRepoWorkgroupOf(stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		} else if id != "" {
			continue
		}
		if pending, err := repository.PendingCrossRepoWorkgroup(stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		} else if pending != "" {
			continue
		}
		if _, err := repository.EnsureManager(ctx, stream, at, serviceActor); err != nil {
			return fmt.Errorf("workstream %s chief-of-staff thread: %w", stream, err)
		}
	}
	return nil
}

func (s *Service) launch(active *activeProject) {
	if active == nil {
		return
	}
	s.log().Info("project started", "project", string(active.id))
	if active.gates == nil {
		active.gates = newGateWorker(s, active)
	}
	s.gateWorkers.Store(active.repository, active.gates)
	if active.climb == nil {
		active.climb = newClimbWorker(s, active)
	}
	s.climbWorkers.Store(active.repository, active.climb)
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
		publishDone := make(chan struct{})
		go func() {
			defer close(publishDone)
			_ = s.publishHearsay(ctx, active.repository)
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
		gatesDone := make(chan struct{})
		go func() {
			active.gates.run(ctx)
			close(gatesDone)
		}()
		climbDone := make(chan struct{})
		go func() {
			active.climb.run(ctx)
			close(climbDone)
		}()
		routesDone := make(chan struct{})
		go func() {
			s.recoverRoutes(ctx, active.repository)
			close(routesDone)
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
		<-publishDone
		<-gatesDone
		<-climbDone
		<-routesDone
		s.gateWorkers.Delete(active.repository)
		s.climbWorkers.Delete(active.repository)
		s.reseeders.Delete(active.id)
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

func (s *Service) openReconciliation(cfg *config.Config) (*trace.Repository, *reconcile.Controller, *pipeline, error) {
	options := s.options.Reconciliation
	directory, err := cfg.Root.ProjectTrace(cfg.Project.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	if exists, err := trace.HasTraceDatabase(directory); err != nil {
		return nil, nil, nil, err
	} else if !exists {
		return nil, nil, nil, nil
	}
	repository, err := trace.Open(cfg.Root, cfg.Project)
	if err != nil {
		if repository != nil {
			repository.Close()
		}
		return nil, nil, nil, err
	}
	observe := s.hub.TraceEvents(cfg.Project.ID)
	repository.Observe(func(c trace.Commit) { observe(c); s.conversationSignal.wake() })
	if s.telemetry.Enabled() {
		repository.ObserveSpans(s.recorder().Observe)
	}
	if options.Worker == "" {
		options.Worker = "local-operations"
	}
	if options.GiveUpWindow == 0 && options.GiveUpWindowFor == nil {
		options.GiveUpWindowFor = s.giveUpWindow
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
		options.Concurrent = ConcurrentOperation
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

func (s *Service) stages(cfg *config.Config, repository *trace.Repository) (*stages, error) {
	options, threads := s.options.Reconciliation, s.options.Threads
	checkClient, cloudClient := s.configuredChecks(cfg), s.configuredCloudChecks(cfg)
	draft := &drafter{S: s.intake().DrafterDependencies(), Repository: repository}
	rounds := &debate{S: s.intake().DebateDependencies(), Repository: repository}
	seals := &sealer{S: s.intake().SealerDependencies(), Repository: repository}
	build := &builder{S: s.intake().BuilderDependencies(), Repository: repository}
	overlap := &overlaps{s: s, repository: repository}
	rules := intake.Charterer{S: s.intake().ChartererDependencies(), Repository: repository}
	land := &foreman{masons: &masons{s: s, cfg: cfg, repository: repository, checks: checkClient, cloudChecks: cloudClient}}
	finals := &delivery.FinalReviewer{S: s.deliveryFor(cfg), Repository: repository}
	publish := &delivery.Publisher{S: s.deliveryFor(cfg), Repository: repository}
	landing := &delivery.Lander{S: s.deliveryFor(cfg), Repository: repository}
	amend := &amendmentDrafter{Drafter: draft}
	amendRounds := amendmentDebate{Debate: rounds}
	budget := budgetSignals{s: s, repository: repository}
	daily := dailyBudget{s: s, repository: repository}
	checks := &checkers{masons: &masons{s: s, cfg: cfg, repository: repository, checks: checkClient, cloudChecks: cloudClient}}
	runner := runnerAdapter{deltas: deltaRunner{S: s.lifecycle()}, turns: options.Adapters[coreadapter.RunnerBoundary], draft: draft, amend: amend, amendRounds: amendRounds, rounds: rounds, finals: finals, checks: checks}
	cleanup := &workspaceCleanup{Cfg: cfg, Repository: repository, Now: s.now}
	hooks := []scheduleHook{{"gates", func(context.Context) error { s.kickGates(repository); return nil }}, {"climb", func(context.Context) error { s.kickClimb(repository); return nil }}, {"workgroup-change", func(ctx context.Context) error { return s.lifecycle().ChangePass(ctx, repository) }}, {"base-refresh", (&baseRefresher{S: s.lifecycle(), Repository: repository}).Pass}, {"base", func(ctx context.Context) error { return s.lifecycle().BaseWaitPass(ctx, repository) }}, {"edge-refresh", func(ctx context.Context) error { return s.lifecycle().EdgeResolverPass(ctx, repository) }}, {"edge", func(ctx context.Context) error { return s.lifecycle().EdgeWaitPass(ctx, repository) }}, {"daily-budget", daily.Pass}, {"loop-guard", loopGuard{s: s, cfg: cfg, repository: repository}.Pass}, {"stall-watchdog", stallWatch{s: s, cfg: cfg, repository: repository}.Run}, {"reservation-waits", func(context.Context) error { s.reservationWaitPass(repository); return nil }}, {"design-start", func(ctx context.Context) error { return s.intake().DesignStartPass(ctx, repository) }}, {"kind-unrecognised", func(ctx context.Context) error { return intake.KindUnrecognisedPass(ctx, repository) }}, {"draft", draft.Pass}, {"budget", budget.Pass}, {"amendment", amend.Pass}, {"amendment-debate", amendRounds.Pass}, {"debate", rounds.Pass}, {"group-approval", func(ctx context.Context) error { return s.groupApprovalPass(ctx, repository) }}, {"seal", seals.Pass}, {"build", build.Pass}, {"overlap", overlap.Pass}, {"charter", rules.Pass}, {"drift", land.Drifts}, {"land", land.Pass}, {"final-review", finals.Pass}, {"publish", publish.Pass}, {"landing-retry", landing.RetryPass}, {"cleanup", cleanup.Pass}, {"archive-on-merge", (&lifecycle.MergeArchiver{S: s.lifecycle(), Repository: repository}).Pass}, {"archive-cleanup", (&archiveCleanup{S: s.lifecycle(), Workspaces: cleanup}).Pass}, {"workgroup-resolution", (&lifecycle.WorkgroupResolver{S: s.lifecycle(), Repository: repository}).Pass}, {"workgroup-project-spec", func(ctx context.Context) error { return s.lifecycle().ProjectSpecDraftPass(ctx, repository) }}, {"workgroup-project-spec-refresh", func(ctx context.Context) error { return s.lifecycle().ProjectSpecRefreshPass(ctx, repository) }}, {"flakes", (&flakeRecorder{s: s, cfg: cfg, repository: repository}).Pass}}
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
		deliver, err := events.New(repository, events.Options{Workstreams: func() ([]config.WorkstreamID, error) { return intake.ObservedWorkstreams(repository) }, Now: options.Now, Window: cfg.EventWindow(), ProgressWindow: cfg.EventProgressWindow(), Profile: s.chiefEventsProfile(cfg), System: s.intake().ChiefEventsPrompt(cfg.Project.ID, repository),
			Skip: s.intake().ChiefEventsSkip(repository), ChiefTarget: func(ctx context.Context, stream config.WorkstreamID) (*trace.Repository, func(), error) {
				target, _, _, close, err := s.chiefRepository(ctx, repository, stream)
				if errors.Is(err, errWorkgroupJoining) {
					return nil, close, fmt.Errorf("%w: %v", events.ErrChiefUnavailable, err)
				}
				return target, close, err
			}})
		if err != nil {
			return nil, err
		}
		units := &masons{s: s, cfg: cfg, repository: repository, checks: checkClient, cloudChecks: cloudClient}
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
		repository: repositoryAdapter{other: options.Adapters[coreadapter.RepositoryBoundary], seals: seals, builds: build, lands: land, publishes: publish, landings: landing}}, nil
}

func ConcurrentOperation(op coreadapter.Operation) bool {
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

func operationClass(op coreadapter.Operation) trace.OperationClass {
	if networkOperation(op) {
		return trace.WorkerOperation
	}
	if op.Boundary == coreadapter.RunnerBoundary {
		switch op.Action {
		case thread.TurnAction:
			in, err := thread.DecodeTurn(op)
			if err == nil && (trace.ManagerLike(in.Agent) || in.Agent == SessionAgentID) {
				return trace.ConversationOperation
			}
			return trace.WorkerOperation
		case RoundAction, AmendmentRoundAction, CheckAction, deltaTurnAction:
			return trace.WorkerOperation
		}
	}
	return trace.WorkflowOperation
}

func stagePriority(op coreadapter.Operation) int {
	switch op.Action {
	case RoundAction, ReplyAction, RedraftAction, AmendmentRoundAction, AmendmentReplyAction:
		return 1
	case DraftAction, AmendmentDraftAction:
		return 2
	}
	return 0
}

func unlock(lock *os.File) {
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	lock.Close()
}
