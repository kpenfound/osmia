package fixturekit

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// SessionReader reads one session through the local API.
type SessionReader interface {
	Session(context.Context, config.WorkstreamID) (api.SessionResponse, error)
}

// AwaitAcknowledged waits until all returned operations are acknowledged.
func AwaitAcknowledged(t *testing.T, fetch func(*testing.T) []trace.OperationRecord) []trace.OperationRecord {
	t.Helper()
	var ops []trace.OperationRecord
	wake, release := Ticks(20 * time.Millisecond)
	defer release()
	Until(t, "the operations to be acknowledged", wake, func() bool {
		ops = fetch(t)
		acknowledged := len(ops) > 0
		for _, o := range ops {
			if !o.Acknowledged {
				acknowledged = false
			}
		}
		return acknowledged
	})
	return ops
}

// AssertCode requires an API error with the given code.
func AssertCode(t *testing.T, err error, code api.Code) {
	t.Helper()
	var response *api.APIError
	if !errors.As(err, &response) || response.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

// AssertGone requires the path to be absent.
func AssertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is still there: %v", path, err)
	}
}

// AwaitSession polls a session until done accepts its view.
func AwaitSession(t *testing.T, c SessionReader, id config.WorkstreamID, done func(api.SessionView) bool) api.SessionView {
	t.Helper()
	var view api.SessionView
	wake, release := Ticks(20 * time.Millisecond)
	defer release()
	Until(t, "the session to settle", wake, func() bool {
		resp, err := c.Session(context.Background(), id)
		Must(t, err)
		view = resp.Session
		return done(view)
	})
	return view
}

// Until re-evaluates cond after every return of wake and fails with what once
// DemoTimeout has passed.
func Until(t testing.TB, what string, wake func() error, cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(DemoTimeout)
	defer deadline.Stop()
	for !cond() {
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", what)
		default:
		}
		if err := wake(); err != nil {
			t.Fatalf("stopped waiting for %s: %v", what, err)
		}
	}
}

// Ticks returns a wake func that returns every interval and the func that
// releases it.
func Ticks(interval time.Duration) (wake func() error, release func()) {
	ticker := time.NewTicker(interval)
	return func() error { <-ticker.C; return nil }, ticker.Stop
}
