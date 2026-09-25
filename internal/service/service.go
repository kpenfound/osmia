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
	"github.com/kpenfound/osmia/internal/pulls"
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
	// Location is the service host's time zone, whose calendar days the daily
	// budget counts. It defaults to the host's local time zone.
	Location *time.Location
	// controls is set by Enforce so the chief-of-staff tools it binds reach
	// the runtime state of the service started with these options.
	controls *runtimeControls
}

// activeProject is the runtime state of the configured project: its open trace
// and the reconciliation loop running against it.
type activeProject struct {
	repository *trace.Repository
	controller *reconcile.Controller
	pipeline   *pipeline
	cancel     context.CancelFunc
	done       chan error
}

type Service struct {
	mu         sync.Mutex // guards cfg, active, pending and reloadErr
	cfg        *config.Config
	active     *activeProject
	pending    error
	reloadErr  *ReloadError
	projectMu  sync.Mutex // serializes project registration, removal and reload
	handInMu   sync.Mutex // serializes hand-ins
	turns      runningTurns
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
	// driftAsked is set once an owner's drift rebase request is recorded,
	// so the next pass reads the drift schedule.
	driftAsked atomic.Bool
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
		return nil, fmt.Errorf("Osmia root is already owned or cannot be locked; stop its service before starting another: %w", err)
	}
	opts.Config.Root = root.String()
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
	s := &Service{options: opts, lock: lock, failures: make(chan error, 1), done: make(chan struct{})}
	if opts.controls != nil {
		opts.controls.service.Store(s)
	}
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
	if active != nil {
		if err = s.recoverSessions(ctx, cfg, active.repository); err != nil {
			listener.Close()
			s.cleanupSocket()
			st.Close()
			return nil, fmt.Errorf("recover thread sessions: %w", err)
		}
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
	}), ReadHeaderTimeout: opts.ReadHeaderTimeout, ReadTimeout: opts.ReadTimeout, WriteTimeout: opts.WriteTimeout,
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
		unlock(s.lock)
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
	return &activeProject{repository: repository, controller: controller, pipeline: p, done: make(chan error, 1)}, nil
}

// admit is the scheduler's gate. It declines every turn of the librarian's
// workstream and of every architect and committee thread, which the service's
// own reconcilers run in their staged views, and of abandoned workstreams, and
// holds the project's other queued turns that a runtime pause in force covers.
// It also holds every mason turn of a unit whose workspace is behind its
// feature branch, so the workspace has no writer when the foreman rebases it.
// The store is read on every pass, so a cleared pause lets held turns run on
// the loop's next periodic pass.
func (s *Service) admit(cfg *config.Config, repository *trace.Repository) func(context.Context, scheduler.Candidate) (bool, error) {
	project := cfg.Project.ID
	librarian := librarianWorkstream(project)
	units := newUnitWorkspaces(cfg)
	return func(ctx context.Context, c scheduler.Candidate) (bool, error) {
		if c.Workstream == librarian || c.Thread.Identity.Role == architectRole || c.Thread.Identity.Role == committeeRole {
			return false, nil
		}
		if gone, err := abandoned(repository, c.Workstream); err != nil || gone {
			return false, err
		}
		st, _ := s.store.Effective()
		if scheduler.Held(st.Pauses, project, c) {
			return false, nil
		}
		if c.Thread.Identity.Role == masonRole && c.Turn.Request.Unit != "" {
			behind, err := units.behind(ctx, c.Workstream, c.Turn.Request.Unit)
			return !behind, err
		}
		return true, nil
	}
}

// priorities returns the runtime priority order in force.
func (s *Service) priorities() []runtime.Priority {
	st, _ := s.store.Effective()
	return st.Priorities
}

// chiefProfile returns the chief of staff's effective profile for a new turn.
func (s *Service) chiefProfile(cfg *config.Config) func() (coreadapter.Profile, error) {
	return func() (coreadapter.Profile, error) {
		_, profile, err := s.chiefOverride(cfg)
		return profile, err
	}
}

// chiefOverride resolves the chief of staff's effective profile name, which
// may be any configured profile, not only one in the role's fallback chain.
func (s *Service) chiefOverride(cfg *config.Config) (string, coreadapter.Profile, error) {
	st, _ := s.store.Effective()
	name := st.Profiles[trace.ChiefOfStaff]
	if name == "" {
		name = cfg.Roles[trace.ChiefOfStaff].Profile
	}
	profile, err := cfg.NamedProfile(name)
	return name, profile, err
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
// trace must open cleanly before the service can report readiness. The runner
// boundary is served by the bound thread reconciler for turns and by the
// service's own reconcilers for knowledge-base extraction and refresh, architect drafts,
// committee rounds and the architect's replies to them, and final reviews, and the repository
// boundary by the service's sealer for sealings, its builder for builds and
// its foreman for landings and rebases and its publisher for publications;
// the daily budget, then the architect controller, then the shed controller, then the sealing
// controller, then the building controller, then the overlap, charter, refresh, drift,
// landing, assembly and publication controllers run at the start of every pass, and the pass reconciles operations in stagePriority order. With
// Options.Threads, outbox events are then delivered to each workstream's
// chief of staff, recorded answers are queued on their askers' threads, the
// mason controller parks, resumes and starts units, and the scheduler runs, whose gate holds turns that a runtime pause covers and mason turns of units behind their feature branch;
// without it, the configured Schedule hook runs instead. The hooks and
// adapters come from the pipeline, which a reload may restage for the next pass.
func (s *Service) openReconciliation(cfg *config.Config) (*trace.Repository, *reconcile.Controller, *pipeline, error) {
	options := s.options.Reconciliation
	directory, err := cfg.Root.ProjectTrace(cfg.Project.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	_, manifestErr := os.Lstat(filepath.Join(directory, "project.json"))
	_, gitErr := os.Lstat(filepath.Join(directory, ".git"))
	if os.IsNotExist(manifestErr) && os.IsNotExist(gitErr) {
		return nil, nil, nil, nil
	}
	repository, err := trace.Open(cfg.Root, cfg.Project)
	if err != nil {
		if repository != nil {
			repository.Close()
		}
		return nil, nil, nil, err
	}
	if options.Worker == "" {
		options.Worker = "local-operations"
	}
	first, err := s.stages(cfg, repository)
	if err != nil {
		repository.Close()
		return nil, nil, nil, err
	}
	p := &pipeline{}
	p.current.Store(first)
	options.Schedule = p.schedule
	adapters := maps.Clone(options.Adapters)
	if adapters == nil {
		adapters = map[coreadapter.OperationBoundary]coreadapter.Reconciler{}
	}
	adapters[coreadapter.RunnerBoundary] = stagedAdapter{p, func(st *stages) coreadapter.Reconciler { return st.runner }}
	adapters[coreadapter.RepositoryBoundary] = stagedAdapter{p, func(st *stages) coreadapter.Reconciler { return st.repository }}
	options.Adapters = adapters
	if options.Priority == nil {
		options.Priority = stagePriority
	}
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
	draft := &drafter{s: s, repository: repository}
	rounds := &debate{s: s, repository: repository}
	seals := &sealer{s: s, repository: repository}
	build := &builder{s: s, repository: repository}
	overlap := &overlaps{s: s, repository: repository}
	rules := charterer{s: s, repository: repository}
	land := &foreman{masons: &masons{s: s, cfg: cfg, repository: repository}}
	refresh := &refresher{extractor: &extractor{s: s, repository: repository}}
	finals := &finalReviewer{s: s, repository: repository}
	publish := &publisher{s: s, repository: repository}
	amend := &amendmentDrafter{drafter: draft}
	amendRounds := amendmentDebate{rounds}
	budget := budgetSignals{s: s, repository: repository}
	daily := dailyBudget{s: s, repository: repository}
	runner := runnerAdapter{turns: options.Adapters[coreadapter.RunnerBoundary], extract: refresh.extractor, refresh: refresh, draft: draft, amend: amend, amendRounds: amendRounds, rounds: rounds, finals: finals}
	hooks := []scheduleHook{{"daily-budget", daily.Pass}, {"draft", draft.Pass}, {"budget", budget.Pass}, {"amendment", amend.Pass}, {"amendment-debate", amendRounds.Pass}, {"debate", rounds.Pass}, {"seal", seals.Pass}, {"build", build.Pass}, {"overlap", overlap.Pass}, {"charter", rules.Pass}, {"refresh", refresh.Pass}, {"drift", land.drifts}, {"land", land.Pass}, {"final-review", finals.Pass}, {"publish", publish.Pass}}
	if threads == nil && options.Schedule != nil {
		hooks = append(hooks, scheduleHook{"configured", options.Schedule})
	}
	if threads != nil {
		bound, err := threads(repository, cfg)
		if err != nil {
			return nil, err
		}
		runner.turns = abandonable{Reconciler: bound, s: s, repository: repository}
		limits := cfg.Capacity
		limits.PerWorkstream = cfg.Project.Capacity.PerWorkstream
		dispatch, err := scheduler.New(repository, scheduler.Options{Now: options.Now, Admit: s.admit(cfg, repository), Capacity: &limits, Priorities: s.priorities})
		if err != nil {
			return nil, err
		}
		deliver, err := events.New(repository, events.Options{Now: options.Now, Window: cfg.EventWindow(), Profile: s.chiefProfile(cfg), System: s.chiefEventsPrompt(cfg.Project.ID, repository),
			Skip: func(stream config.WorkstreamID) (bool, error) { return abandoned(repository, stream) }})
		if err != nil {
			return nil, err
		}
		units := &masons{s: s, cfg: cfg, repository: repository}
		reviews := &reviewers{masons: units}
		hooks = append(hooks, scheduleHook{"events", deliver.Pass}, scheduleHook{"answers", s.answers(cfg, repository).Pass}, scheduleHook{"reviews", reviews.Pass}, scheduleHook{"masons", units.Pass}, scheduleHook{"dispatch", dispatch.Pass})
	}
	return &stages{cfg: cfg, hooks: hooks, runner: runner,
		repository: repositoryAdapter{other: options.Adapters[coreadapter.RepositoryBoundary], seals: seals, builds: build, lands: land, publishes: publish}}, nil
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
