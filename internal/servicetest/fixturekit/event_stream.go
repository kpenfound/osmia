package fixturekit

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

// EventStream reads c's event stream until the test ends, and returns the
// events received and a channel that yields Events' result once it returns.
func EventStream(t *testing.T, c interface {
	Events(context.Context, func(api.Event) error) error
}) (<-chan api.Event, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	events, done := make(chan api.Event, 1024), make(chan error, 1)
	go func() {
		done <- c.Events(ctx, func(e api.Event) error {
			events <- e
			return nil
		})
	}()
	t.Cleanup(cancel)
	return events, done
}

// NextEvent returns the stream's next event.
func NextEvent(t *testing.T, events <-chan api.Event) api.Event {
	t.Helper()
	select {
	case e := <-events:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("no event within 10s")
		return api.Event{}
	}
}

// AwaitEvents reads the stream until it has seen every one of want, in any
// order, and returns everything it read.
func AwaitEvents(t *testing.T, events <-chan api.Event, want ...api.Event) []api.Event {
	t.Helper()
	var seen []api.Event
	for missing := slices.Clone(want); len(missing) > 0; {
		e := NextEvent(t, events)
		seen = append(seen, e)
		missing = slices.DeleteFunc(missing, func(w api.Event) bool { return w == e })
	}
	return seen
}
