package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/redact"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Defaults of the sink's queue and export loop.
const (
	DefaultQueueSize     = 4096
	DefaultBatchSize     = 256
	DefaultFlushInterval = 2 * time.Second
	DefaultExportTimeout = 10 * time.Second
	// ShutdownWait is how long the service waits for pending spans on shutdown.
	ShutdownWait = 5 * time.Second
	// failureLogInterval spaces the log lines about failed exports.
	failureLogInterval = time.Minute
)

// ErrShutdownTimeout is returned by Shutdown when pending spans were not all
// sent within the wait.
var ErrShutdownTimeout = errors.New("telemetry: pending spans were not flushed before the shutdown wait ended")

// Options tune a Sink; the zero value gives the defaults.
type Options struct {
	Logger *slog.Logger
	// Getenv reads the variable [telemetry] headers_env names. It defaults to
	// os.Getenv. It is the only environment variable the package reads.
	Getenv func(string) string
	// Version is the service version, the resource's service.version.
	Version string
	// Exporter replaces the exporter built from the configuration, and turns
	// the sink on even when no endpoint is configured. It is for tests, with
	// an in-memory exporter; the sink never shuts it down.
	Exporter sdktrace.SpanExporter
	// QueueSize bounds the spans waiting to be exported; a span emitted while
	// the queue is full is dropped.
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	ExportTimeout time.Duration
}

// Stats counts what a Sink did with the spans it was given.
type Stats struct {
	// Exported spans were handed to the exporter and accepted.
	Exported int64
	// Failed spans were handed to the exporter and refused, or lost to a
	// failed or abandoned export.
	Failed int64
	// Dropped spans found the queue full or the sink shut down.
	Dropped int64
	// Rejected spans were unusable: no valid name or IDs, or an end before the start.
	Rejected int64
	// DroppedAttrs counts attributes outside the allow-list, repeated keys
	// and events without a valid name or time.
	DroppedAttrs int64
}

// Sink queues spans and exports them in the background. A nil or disabled Sink
// accepts spans and discards them. Every method is safe for concurrent use.
type Sink struct {
	exporter    sdktrace.SpanExporter
	ownExporter bool
	resource    *resource.Resource
	logger      *slog.Logger
	secrets     []string

	queue         chan sdktrace.ReadOnlySpan
	flushes       chan chan struct{}
	stop          chan struct{}
	exited        chan struct{}
	batchSize     int
	flushInterval time.Duration
	exportTimeout time.Duration

	exportCtx    context.Context
	abortExports context.CancelFunc
	stopOnce     sync.Once
	closed       atomic.Bool
	lastFailLog  atomic.Int64

	exported, failed, dropped, rejected, droppedAttrs atomic.Int64
}

// New builds the sink cfg describes. With no endpoint and no Options.Exporter it
// builds no exporter, starts nothing and returns a disabled sink. Otherwise the
// exporter is built from cfg alone, with the headers read from the variable
// headers_env names; a problem building it is returned and no sink is. The
// header values are registered for redaction and appear in no error or log.
func New(cfg config.Telemetry, opts Options) (*Sink, error) {
	if !cfg.Enabled() && opts.Exporter == nil {
		return &Sink{}, nil
	}
	var secrets []string
	exporter, owned := opts.Exporter, false
	if exporter == nil {
		headers, err := headersOf(cfg, opts.Getenv)
		if err != nil {
			return nil, err
		}
		for _, v := range headers {
			if v != "" {
				secrets = append(secrets, v)
				redact.Register(v)
			}
		}
		if exporter, err = NewExporter(context.Background(), cfg, headers); err != nil {
			return nil, errors.New(redact.Secrets(err.Error(), secrets...))
		}
		owned = true
	}
	s := &Sink{
		exporter:      exporter,
		ownExporter:   owned,
		resource:      resourceOf(opts.Version),
		logger:        opts.Logger,
		secrets:       secrets,
		queue:         make(chan sdktrace.ReadOnlySpan, positive(opts.QueueSize, DefaultQueueSize)),
		flushes:       make(chan chan struct{}),
		stop:          make(chan struct{}),
		exited:        make(chan struct{}),
		batchSize:     positive(opts.BatchSize, DefaultBatchSize),
		flushInterval: positiveDuration(opts.FlushInterval, DefaultFlushInterval),
		exportTimeout: positiveDuration(opts.ExportTimeout, DefaultExportTimeout),
	}
	s.exportCtx, s.abortExports = context.WithCancel(context.Background())
	go s.run()
	return s, nil
}

func positive(n, fallback int) int {
	if n > 0 {
		return n
	}
	return fallback
}

func positiveDuration(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// headersOf reads cfg's export headers from the environment variable it names.
// A variable that is named but unset or empty sends no headers.
func headersOf(cfg config.Telemetry, getenv func(string) string) (map[string]string, error) {
	if cfg.HeadersEnv == "" {
		return nil, nil
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	raw := getenv(cfg.HeadersEnv)
	if raw == "" {
		return nil, nil
	}
	return ParseHeaders(cfg.HeadersEnv, raw)
}

// resourceOf is the resource of every span: service.name and, when known, the
// service version. It is built by hand and never from the environment, so
// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES have no effect.
func resourceOf(version string) *resource.Resource {
	attrs := []attribute.KeyValue{attribute.String("service.name", Namespace)}
	if version != "" {
		attrs = append(attrs, attribute.String("service.version", version))
	}
	return resource.NewSchemaless(attrs...)
}

// Enabled reports whether the sink exports spans.
func (s *Sink) Enabled() bool { return s != nil && s.exporter != nil }

// Emit queues span for export. It never blocks, never does I/O and never
// returns an error: a span that is unusable, finds the queue full or comes after
// shutdown is counted in Stats and dropped.
func (s *Sink) Emit(span Span) {
	if !s.Enabled() {
		return
	}
	snapshot, droppedAttrs, ok := span.snapshot(s.resource)
	s.droppedAttrs.Add(int64(droppedAttrs))
	if !ok {
		s.rejected.Add(1)
		return
	}
	if s.closed.Load() {
		s.dropped.Add(1)
		return
	}
	select {
	case s.queue <- snapshot:
	default:
		s.dropped.Add(1)
	}
}

// Stats returns the counts so far.
func (s *Sink) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	return Stats{Exported: s.exported.Load(), Failed: s.failed.Load(), Dropped: s.dropped.Load(), Rejected: s.rejected.Load(), DroppedAttrs: s.droppedAttrs.Load()}
}

// Flush exports every span queued before the call, waiting until they are
// exported or ctx ends.
func (s *Sink) Flush(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	ack := make(chan struct{})
	select {
	case s.flushes <- ack:
	case <-s.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops accepting spans, exports the pending ones and shuts the
// exporter down, waiting no longer than ctx allows. When the wait ends first,
// in-flight exports are abandoned and ErrShutdownTimeout is returned.
func (s *Sink) Shutdown(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	s.stopOnce.Do(func() {
		s.closed.Store(true)
		close(s.stop)
	})
	select {
	case <-s.exited:
	case <-ctx.Done():
		s.abortExports()
		return ErrShutdownTimeout
	}
	s.abortExports()
	if !s.ownExporter {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.exporter.Shutdown(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			return errors.New(redact.Secrets(err.Error(), s.secrets...))
		}
		return nil
	case <-ctx.Done():
		return ErrShutdownTimeout
	}
}

// Close shuts the sink down within ShutdownWait.
func (s *Sink) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownWait)
	defer cancel()
	return s.Shutdown(ctx)
}

func (s *Sink) run() {
	defer close(s.exited)
	var batch []sdktrace.ReadOnlySpan
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()
	send := func() {
		if len(batch) > 0 {
			s.export(batch)
			batch = nil
		}
	}
	drain := func() {
		for {
			select {
			case span := <-s.queue:
				batch = append(batch, span)
				if len(batch) >= s.batchSize {
					send()
				}
			default:
				send()
				return
			}
		}
	}
	for {
		select {
		case span := <-s.queue:
			batch = append(batch, span)
			if len(batch) >= s.batchSize {
				send()
			}
		case <-ticker.C:
			send()
		case ack := <-s.flushes:
			drain()
			close(ack)
		case <-s.stop:
			drain()
			return
		}
	}
}

// export hands batch to the exporter within the export timeout. A refusal, a
// timeout and a panic all drop the batch; none stops the sink.
func (s *Sink) export(batch []sdktrace.ReadOnlySpan) {
	ctx, cancel := context.WithTimeout(s.exportCtx, s.exportTimeout)
	defer cancel()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("exporter panicked: %v", r)
			}
		}()
		return s.exporter.ExportSpans(ctx, batch)
	}()
	if err == nil {
		s.exported.Add(int64(len(batch)))
		return
	}
	s.failed.Add(int64(len(batch)))
	s.logFailure(err, len(batch))
}

// logFailure logs a failed export, at most once per failureLogInterval, with
// every header value removed from the error.
func (s *Sink) logFailure(err error, spans int) {
	if s.logger == nil {
		return
	}
	now := time.Now().UnixNano()
	last := s.lastFailLog.Load()
	if last != 0 && now-last < int64(failureLogInterval) {
		return
	}
	if !s.lastFailLog.CompareAndSwap(last, now) {
		return
	}
	s.logger.Warn("telemetry export failed; spans dropped", "component", "telemetry", "spans", spans, "error", redact.Secrets(err.Error(), s.secrets...))
}
