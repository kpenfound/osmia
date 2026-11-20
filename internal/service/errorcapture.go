package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/errorreport"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/thread"
)

// errorQueueLimit is how many captured errors wait for the reporter's worker
// before the oldest is dropped.
const errorQueueLimit = 256

// The log messages the reporter picks errors from.
const (
	apiRequestMessage      = "api request handled"
	operationFailedMessage = "operation failed"
)

// errorCapture is the reporter's tee of the service's structured log. Its
// handler sees every record the service writes and queues the error records of
// the enabled sources; it never blocks the caller beyond a short critical
// section, and it holds nothing while the reporter is off.
type errorCapture struct {
	enabled atomic.Bool
	sources atomic.Uint32
	wake    chan struct{}
	limit   int
	now     func() time.Time

	mu      sync.Mutex
	queue   []queuedError
	epoch   uint64
	dropped int
}

// queuedError is a captured error and the epoch of the queue it waits in.
type queuedError struct {
	event errorreport.Event
	epoch uint64
}

func newErrorCapture(limit int, now func() time.Time) *errorCapture {
	if limit <= 0 {
		limit = errorQueueLimit
	}
	if now == nil {
		now = time.Now
	}
	return &errorCapture{wake: make(chan struct{}, 1), limit: limit, now: now}
}

const (
	sourceAPI uint32 = 1 << iota
	sourceOperation
	sourceGitHub
)

func sourceBit(source errorreport.Source) uint32 {
	switch source {
	case errorreport.SourceAPI:
		return sourceAPI
	case errorreport.SourceOperation:
		return sourceOperation
	case errorreport.SourceGitHub:
		return sourceGitHub
	}
	return 0
}

// configure turns capturing on for the configured sources, or off. Turning it
// off, or changing nothing about it while it is already off, empties the queue,
// so an error from before the reporter was enabled is never filed.
func (c *errorCapture) configure(e config.ErrorReports) {
	if !e.Enabled {
		c.enabled.Store(false)
		c.clear()
		c.signal()
		return
	}
	var bits uint32
	for _, name := range e.Sources {
		bits |= sourceBit(errorreport.Source(name))
	}
	c.sources.Store(bits)
	c.enabled.Store(true)
	c.signal()
}

func (c *errorCapture) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// clear drops everything queued and every uncounted drop. A worker holding a
// queued error from before sees its epoch is no longer current.
func (c *errorCapture) clear() {
	c.mu.Lock()
	c.queue = nil
	c.dropped = 0
	c.epoch++
	c.mu.Unlock()
}

// push queues ev, dropping and counting the oldest queued error when the queue
// is full.
func (c *errorCapture) push(ev errorreport.Event) {
	c.mu.Lock()
	if len(c.queue) >= c.limit {
		c.queue = append(c.queue[:0], c.queue[1:]...)
		c.dropped++
	}
	c.queue = append(c.queue, queuedError{event: ev, epoch: c.epoch})
	c.mu.Unlock()
	c.signal()
}

// head returns the oldest queued error without removing it.
func (c *errorCapture) head() (queuedError, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		return queuedError{}, false
	}
	return c.queue[0], true
}

// consume removes the oldest queued error if it is q, and reports whether q
// was still queued: false means the queue was emptied since q was read.
func (c *errorCapture) consume(q queuedError) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if q.epoch != c.epoch {
		return false
	}
	if len(c.queue) > 0 {
		c.queue = append(c.queue[:0], c.queue[1:]...)
	}
	return true
}

// takeDropped returns the errors dropped from a full queue since the last
// call.
func (c *errorCapture) takeDropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.dropped
	c.dropped = 0
	return n
}

// queued is the number of errors waiting.
func (c *errorCapture) queued() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// wrap returns inner teed to c.
func (c *errorCapture) wrap(inner slog.Handler) slog.Handler {
	return &captureHandler{inner: inner, capture: c}
}

// captureHandler passes every record to the logger it wraps and offers it to
// the capture.
type captureHandler struct {
	inner   slog.Handler
	capture *errorCapture
	attrs   []slog.Attr
	grouped bool
}

// Enabled is true for every level while capturing, because the API request
// record that carries a 5xx status is logged at Info.
func (h *captureHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.capture.enabled.Load() || h.inner.Enabled(ctx, level)
}

func (h *captureHandler) Handle(ctx context.Context, rec slog.Record) error {
	if h.capture.enabled.Load() && !h.grouped {
		h.capture.observe(h.attrs, rec)
	}
	if h.inner.Enabled(ctx, rec.Level) {
		return h.inner.Handle(ctx, rec)
	}
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &captureHandler{inner: h.inner.WithAttrs(attrs), capture: h.capture, grouped: h.grouped}
	if !h.grouped {
		next.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	}
	return next
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &captureHandler{inner: h.inner.WithGroup(name), capture: h.capture, grouped: true}
}

// observe queues the record if it describes an error of an enabled source. It
// reads only the allowlisted attributes and never panics into the logging
// caller.
func (c *errorCapture) observe(pre []slog.Attr, rec slog.Record) {
	defer func() { _ = recover() }()
	candidate := rec.Message == apiRequestMessage || rec.Message == operationFailedMessage || rec.Level >= slog.LevelError
	if !candidate {
		return
	}
	attrs := make(map[string]slog.Value, 12)
	for _, a := range pre {
		attrs[a.Key] = a.Value.Resolve()
	}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Resolve()
		return true
	})
	ev, ok := classifyError(rec, attrs, c.now())
	if !ok || c.sources.Load()&sourceBit(ev.Source) == 0 {
		return
	}
	c.push(ev)
}

// classifyError says whether a record is an error the reporter files and
// extracts its allowlisted fields. The reporter's own GitHub calls, git remote
// operations, model-provider turn errors and Jev failures are not.
func classifyError(rec slog.Record, attrs map[string]slog.Value, when time.Time) (errorreport.Event, bool) {
	text := func(key string) string {
		if v, ok := attrs[key]; ok && v.Kind() == slog.KindString {
			return v.String()
		}
		return ""
	}
	number := func(key string) int {
		if v, ok := attrs[key]; ok {
			switch v.Kind() {
			case slog.KindInt64:
				return int(v.Int64())
			case slog.KindUint64:
				return int(v.Uint64())
			}
		}
		return 0
	}
	ev := errorreport.Event{Time: when.UTC(), ProjectID: text("project"), WorkstreamID: text("workstream"), UnitID: text("unit")}
	switch {
	case rec.Message == apiRequestMessage:
		status := number("status")
		if status < http.StatusInternalServerError {
			return ev, false
		}
		ev.Source, ev.Status = errorreport.SourceAPI, status
		ev.Template = routeTemplate(text("method"), text("path"))
		ev.Message = http.StatusText(status)
		if ev.Message == "" {
			ev.Message = "server error"
		}
		ev.ProjectID, ev.WorkstreamID, ev.UnitID = "", "", ""
		return ev, true
	case rec.Message == operationFailedMessage && rec.Level >= slog.LevelError:
		// Agent turns, whose model-provider errors are not reported, never
		// log an operation failure; the action is excluded all the same.
		action := text("action")
		if action == "" || action == thread.TurnAction {
			return ev, false
		}
		ev.Source, ev.Template, ev.Message = errorreport.SourceOperation, action, text("error")
		return ev, true
	case rec.Level >= slog.LevelError && text(ghlog.Marker) == ghlog.MarkerValue:
		if v, ok := attrs[ghlog.ReporterField]; ok && v.Kind() == slog.KindBool && v.Bool() {
			return ev, false
		}
		method, path := text("method"), text("path")
		if method == "" || path == "" {
			return ev, false
		}
		ev.Source, ev.Template, ev.Status = errorreport.SourceGitHub, method+" "+path, number("status")
		ev.Message = text("action") + ": " + firstNonEmpty(text("error"), rec.Message)
		if v, ok := attrs["no_response"]; ok && v.Kind() == slog.KindBool && v.Bool() {
			ev.Code = "no_response"
		}
		return ev, true
	}
	return ev, false
}

// routeTemplate names the endpoint of an API request as its method and the
// route pattern from Routes, never the concrete path.
func routeTemplate(method, path string) string {
	if t, ok := matchRoute(method, path); ok {
		return method + " " + t
	}
	if method == "" {
		method = http.MethodGet
	}
	return method + " unrouted"
}

var routeSegments = func() []routePattern {
	out := make([]routePattern, 0, len(Routes))
	for _, r := range Routes {
		out = append(out, routePattern{method: r.Method, path: r.Path, segments: strings.Split(strings.Trim(r.Path, "/"), "/")})
	}
	return out
}()

type routePattern struct {
	method, path string
	segments     []string
}

// matchRoute finds the route of Routes that serves method and path and returns
// its path template. Literal segments win over parameters.
func matchRoute(method, path string) (string, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	best, bestLiterals := "", -1
	for _, r := range routeSegments {
		if r.method != method || len(r.segments) != len(segs) {
			continue
		}
		literals, ok := 0, true
		for i, want := range r.segments {
			switch {
			case strings.HasPrefix(want, "{") && strings.HasSuffix(want, "}"):
			case want == segs[i]:
				literals++
			default:
				ok = false
			}
			if !ok {
				break
			}
		}
		if ok && literals > bestLiterals {
			best, bestLiterals = r.path, literals
		}
	}
	return best, bestLiterals >= 0
}
