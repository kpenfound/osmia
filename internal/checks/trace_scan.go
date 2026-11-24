package checks

import (
	"bytes"
	"context"
	"sync"

	"github.com/kpenfound/osmia/internal/redact"
)

// TraceObserver is told the trace link of a check run as soon as Dagger prints
// it, and again whenever a later link replaces it. It runs on the goroutine
// that copies Dagger's output, so it must return promptly and must not call
// back into the run.
type TraceObserver func(url string)

type traceObserverKey struct{}

// WithTraceObserver returns a context whose check runs report their trace link
// to observe while they run. DaggerChecks and RunSelectionWithFallback both
// honour it, and observe sees each distinct link once, in the order printed.
func WithTraceObserver(ctx context.Context, observe TraceObserver) context.Context {
	if observe == nil {
		return ctx
	}
	return context.WithValue(ctx, traceObserverKey{}, observe)
}

func traceObserverFrom(ctx context.Context) TraceObserver {
	observe, _ := ctx.Value(traceObserverKey{}).(TraceObserver)
	return observe
}

func distinctTraces(ctx context.Context) (context.Context, func(url string)) {
	next := traceObserverFrom(ctx)
	if next == nil {
		return ctx, func(string) {}
	}
	var mu sync.Mutex
	var last string
	see := func(url string) {
		mu.Lock()
		defer mu.Unlock()
		if url == "" || url == last {
			return
		}
		last = url
		next(url)
	}
	return WithTraceObserver(ctx, see), see
}

const traceLineLimit = 64 << 10

type traceScanner struct {
	mu      sync.Mutex
	partial []byte
	url     string
	observe TraceObserver
}

func newTraceScanner(found string, observe TraceObserver) *traceScanner {
	return &traceScanner{url: found, observe: observe}
}

func (s *traceScanner) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	for i, b := range p {
		if b != '\n' && b != '\r' {
			continue
		}
		s.line(p[start:i])
		start = i + 1
	}
	s.partial = append(s.partial, p[start:]...)
	if len(s.partial) > traceLineLimit {
		s.scan(s.partial)
		s.partial = append(s.partial[:0], s.partial[len(s.partial)-traceLineLimit/2:]...)
	}
	return len(p), nil
}

func (s *traceScanner) line(tail []byte) {
	if len(s.partial) == 0 {
		s.scan(tail)
		return
	}
	s.partial = append(s.partial, tail...)
	s.scan(s.partial)
	s.partial = s.partial[:0]
}

func (s *traceScanner) scan(line []byte) {
	if len(line) == 0 {
		return
	}
	found := cloudTraceURL(redact.Secrets(string(line), redact.KnownSecrets()...))
	if found == "" || found == s.url {
		return
	}
	s.url = found
	if s.observe != nil {
		s.observe(found)
	}
}

func (s *traceScanner) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scan(s.partial)
	s.partial = s.partial[:0]
}

func (s *traceScanner) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

type scanned struct {
	output *bytes.Buffer
	scan   *traceScanner
}

func (w *scanned) Write(p []byte) (int, error) {
	n, err := w.output.Write(p)
	w.scan.Write(p[:n])
	return n, err
}
