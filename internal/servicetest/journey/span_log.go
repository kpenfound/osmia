package journey

import (
	"sync"

	"github.com/kpenfound/osmia/internal/router"
)

// SpanLog collects the routed spans of a service's router.
type SpanLog struct {
	mu    sync.Mutex
	spans []router.SpanRoute
}

// Add records one routed span. Assign it to the router's Spans hook.
func (l *SpanLog) Add(s router.SpanRoute) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spans = append(l.spans, s)
}

// All returns every span recorded so far.
func (l *SpanLog) All() []router.SpanRoute {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]router.SpanRoute(nil), l.spans...)
}

// Of returns the spans recorded for the trigger.
func (l *SpanLog) Of(trigger string) []router.SpanRoute {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []router.SpanRoute
	for _, s := range l.spans {
		if s.Trigger == trigger {
			out = append(out, s)
		}
	}
	return out
}
