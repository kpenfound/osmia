package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/events"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

type Options struct {
	Config config.Options
	Build  Identity
	// Workstreams supplies known identities when no trace exists. An existing
	// trace supplies its validated workstream manifests.
	Workstreams     []config.WorkstreamID
	ShutdownTimeout time.Duration
	Reconciliation  reconcile.Options
	// Threads binds the runner-boundary reconciler to the trace this service
	// owns. It is called each time a project's trace opens, at startup and when
	// a project is added, and replaces any runner adapter in Reconciliation.
	// With Threads set, the service also dispatches every queued workstream turn
	// on its own, never more than one turn per thread in flight and within the
	// configured capacity, replacing Reconciliation.Schedule. A runtime pause
	// holds new turns on the threads it covers, except chief-of-staff turns;
	// clearing it lets them run. Outbox events are delivered to each
	// workstream's chief of staff as queued turns, one per event window.
	// Callers must not close the repository.
	Threads func(*trace.Repository) (coreadapter.Reconciler, error)
	// Issues fetches issue URLs handed in. It defaults to the GitHub REST API
	// with the service's GITHUB_TOKEN environment variable, which no session
	// receives.
	Issues issues.Client
}

// activeProject is the runtime state of the configured project: its open trace
// and the reconciliation loop running against it.
type activeProject struct {
	repository *trace.Repository
	controller *reconcile.Controller
	cancel     context.CancelFunc
	done       chan error
}

type Service struct {
	mu         sync.Mutex // guards cfg, active and pending
	cfg        *config.Config
	active     *activeProject
	pending    error
	projectMu  sync.Mutex // serializes project registration and removal
	handInMu   sync.Mutex // serializes hand-ins
	options    Options
	store      *runtime.Store
	lock       *os.File
	listener   *net.UnixListener
	socketInfo os.FileInfo
	server     *http.Server
	lifetime   context.Context
	cancel     context.CancelFunc
	failures   chan error
	done       chan struct{}
	err        error
	ready      atomic.Bool
	requests   sync.WaitGroup
	boundary   func(string) error
}

// Start loads state and binds before returning. Wait joins shutdown and cleanup.
// A configuration without an active project starts an idle service that accepts
// project registration; an interrupted registration is completed first.
func Start(ctx context.Context, opts Options) (_ *Service, err error) {
	root, err := config.ResolveRoot(opts.Config.Root, opts.Config.Home)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root.String(), ".service.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("open root ownership lock: %w", err)
	}
	defer func() {
		if err != nil {
			lock.Close()
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
		return nil, fmt.Errorf("Osmia root is already owned or cannot be locked; stop its service before starting another: %w", err)
	}
	opts.Config.Root = root.String()
	if opts.Issues == nil {
		opts.Issues = issues.GitHub{Token: os.Getenv("GITHUB_TOKEN"), HTTP: &http.Client{Timeout: 30 * time.Second}}
	}
	cfg, err := config.Load(opts.Config)
	if err != nil {
		return nil, err
	}
	s := &Service{options: opts, lock: lock, failures: make(chan error, 1), done: make(chan struct{})}
	cfg, s.pending = s.recoverPending(ctx, cfg)
	active, err := s.open(cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil && active != nil {
			active.repository.Close()
		}
	}()
	workstreams := opts.Workstreams
	if active != nil {
		workstreams, err = active.repository.Workstreams()
		if err != nil {
			return nil, err
		}
	}
	if !cfg.HasProject() {
		workstreams = nil
	}
	st, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: workstreams})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			st.Close()
		}
	}()
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
	s.cfg, s.active, s.store, s.listener, s.socketInfo = cfg, active, st, listener, info
	if err = os.Chmod(cfg.Listen.Socket, 0600); err != nil {
		s.cleanupSocket()
		return nil, err
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 5 * time.Second
	}
	s.lifetime, s.cancel = context.WithCancel(ctx)
	s.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		defer s.requests.Done()
		if !s.ready.Load() {
			fail(w, Unavailable)
			return
		}
		s.handle(w, r)
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return s.lifetime }}
	s.ready.Store(true)
	s.launch(active)
	go func() {
		served := make(chan error, 1)
		go func() { served <- s.server.Serve(listener) }()
		select {
		case <-s.lifetime.Done():
		case e := <-s.failures:
			s.err = e
		case e := <-served:
			if !errors.Is(e, http.ErrServerClosed) {
				s.err = e
			}
			served = nil
		}
		s.ready.Store(false)
		drain, stop := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
		if e := s.server.Shutdown(drain); e != nil {
			s.server.Close()
		}
		stop()
		s.cancel()
		if served != nil {
			if e := <-served; !errors.Is(e, http.ErrServerClosed) {
				s.err = e
			}
		}
		s.requests.Wait()
		s.mu.Lock()
		active := s.active
		s.active = nil
		s.mu.Unlock()
		s.err = errors.Join(s.err, s.stop(active))
		s.cleanupSocket()
		s.store.Close()
		s.lock.Close()
		close(s.done)
	}()
	return s, nil
}
func (s *Service) Socket() string { return s.cfg.Listen.Socket }
func (s *Service) Wait() error    { <-s.done; return s.err }
func (s *Service) Close() error   { s.cancel(); return s.Wait() }

// current returns the loaded configuration; project registration replaces it.
func (s *Service) current() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
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
	if info, err := os.Lstat(s.cfg.Listen.Socket); err == nil && os.SameFile(info, s.socketInfo) {
		os.Remove(s.cfg.Listen.Socket)
	}
}

// open opens the configured project's existing trace and its reconciliation
// controller. Without a configured project, or without a trace for it, the
// service runs idle: trace creation belongs to project registration.
func (s *Service) open(cfg *config.Config) (*activeProject, error) {
	if !cfg.HasProject() {
		return nil, nil
	}
	repository, controller, err := openReconciliation(cfg, s.options.Reconciliation, s.options.Threads, s.unpaused(cfg.Project.ID), s.chiefProfile(cfg))
	if err != nil || repository == nil {
		return nil, err
	}
	if err := ensureChiefsOfStaff(context.Background(), repository, time.Now().UTC()); err != nil {
		repository.Close()
		return nil, err
	}
	return &activeProject{repository: repository, controller: controller, done: make(chan error, 1)}, nil
}

// unpaused admits the project's queued turns that no runtime pause in force
// holds. The store is read on every pass, so a cleared pause lets held turns
// run on the loop's next periodic pass.
func (s *Service) unpaused(project config.ProjectID) func(context.Context, scheduler.Candidate) (bool, error) {
	return func(_ context.Context, c scheduler.Candidate) (bool, error) {
		st, _ := s.store.Effective()
		return !scheduler.Held(st.Pauses, project, c), nil
	}
}

// chiefProfile returns the chief of staff's effective profile for a new turn.
func (s *Service) chiefProfile(cfg *config.Config) func() (coreadapter.Profile, error) {
	return func() (coreadapter.Profile, error) {
		st, _ := s.store.Effective()
		name := st.Profiles[trace.ChiefOfStaff]
		if name == "" {
			name = cfg.Roles[trace.ChiefOfStaff].Profile
		}
		return cfg.NamedProfile(name)
	}
}

// ensureChiefsOfStaff gives every workstream in the trace its chief-of-staff
// thread, leaving existing threads untouched.
func ensureChiefsOfStaff(ctx context.Context, repository *trace.Repository, at time.Time) error {
	streams, err := repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
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
	ctx, cancel := context.WithCancel(s.lifetime)
	active.cancel = cancel
	go func() {
		err := active.controller.Run(ctx)
		active.done <- err
		if err != nil && !errors.Is(err, context.Canceled) {
			select {
			case s.failures <- err:
			default:
			}
		}
	}()
}

// stop joins the controller and releases the trace. It reports loop failures
// other than cancellation.
func (s *Service) stop(active *activeProject) error {
	if active == nil {
		return nil
	}
	var err error
	if active.cancel != nil {
		active.cancel()
		if e := <-active.done; e != nil && !errors.Is(e, context.Canceled) {
			err = e
		}
	}
	return errors.Join(err, active.repository.Close())
}

// openReconciliation leaves trace creation to project registration; an existing
// trace must open cleanly before the service can report readiness.
func openReconciliation(cfg *config.Config, options reconcile.Options, threads func(*trace.Repository) (coreadapter.Reconciler, error), admit func(context.Context, scheduler.Candidate) (bool, error), profile func() (coreadapter.Profile, error)) (*trace.Repository, *reconcile.Controller, error) {
	directory, err := cfg.Root.ProjectTrace(cfg.Project.ID)
	if err != nil {
		return nil, nil, err
	}
	_, manifestErr := os.Lstat(filepath.Join(directory, "project.json"))
	_, gitErr := os.Lstat(filepath.Join(directory, ".git"))
	if os.IsNotExist(manifestErr) && os.IsNotExist(gitErr) {
		return nil, nil, nil
	}
	repository, err := trace.Open(cfg.Root, cfg.Project)
	if err != nil {
		if repository != nil {
			repository.Close()
		}
		return nil, nil, err
	}
	if options.Worker == "" {
		options.Worker = "local-operations"
	}
	if threads != nil {
		runner, err := threads(repository)
		if err != nil {
			repository.Close()
			return nil, nil, err
		}
		adapters := maps.Clone(options.Adapters)
		if adapters == nil {
			adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{}
		}
		adapters[coreadapter.RunnerBoundary] = runner
		options.Adapters = adapters
		limits := cfg.Capacity
		limits.PerWorkstream = cfg.Project.Capacity.PerWorkstream
		dispatch, err := scheduler.New(repository, scheduler.Options{Now: options.Now, Admit: admit, Capacity: &limits})
		if err != nil {
			repository.Close()
			return nil, nil, err
		}
		deliver, err := events.New(repository, events.Options{Now: options.Now, Window: cfg.EventWindow(), Profile: profile})
		if err != nil {
			repository.Close()
			return nil, nil, err
		}
		options.Schedule = func(ctx context.Context) error {
			if err := deliver.Pass(ctx); err != nil {
				return err
			}
			return dispatch.Pass(ctx)
		}
	}
	controller, err := reconcile.New(repository, options)
	if err != nil {
		repository.Close()
		return nil, nil, err
	}
	return repository, controller, nil
}
