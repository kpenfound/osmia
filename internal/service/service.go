package service

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/kpenfound/osmia/internal/runtime"
)

type Options struct {
	Config config.Options
	Build  Identity
	// Workstreams are supplied by the persisted record repository, never a scan.
	Workstreams     []config.WorkstreamID
	ShutdownTimeout time.Duration
}
type Service struct {
	cfg        *config.Config
	options    Options
	store      *runtime.Store
	lock       *os.File
	listener   *net.UnixListener
	socketInfo os.FileInfo
	server     *http.Server
	cancel     context.CancelFunc
	done       chan struct{}
	err        error
	ready      atomic.Bool
	requests   sync.WaitGroup
}

// Start loads state and binds before returning. Wait joins shutdown and cleanup.
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
	cfg, err := config.Load(opts.Config)
	if err != nil {
		return nil, err
	}
	st, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: opts.Workstreams})
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
	s := &Service{cfg: cfg, options: opts, store: st, lock: lock, listener: listener, socketInfo: info, done: make(chan struct{})}
	if err = os.Chmod(cfg.Listen.Socket, 0600); err != nil {
		s.cleanupSocket()
		return nil, err
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 5 * time.Second
	}
	lifetime, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		defer s.requests.Done()
		if !s.ready.Load() {
			fail(w, Unavailable)
			return
		}
		s.handle(w, r)
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return lifetime }}
	s.ready.Store(true)
	go func() {
		served := make(chan error, 1)
		go func() { served <- s.server.Serve(listener) }()
		select {
		case <-lifetime.Done():
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
		cancel()
		if served != nil {
			if e := <-served; !errors.Is(e, http.ErrServerClosed) {
				s.err = e
			}
		}
		s.requests.Wait()
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
