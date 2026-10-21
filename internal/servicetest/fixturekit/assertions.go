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
	deadline := time.Now().Add(DemoTimeout)
	for {
		ops := fetch(t)
		acknowledged := len(ops) > 0
		for _, o := range ops {
			if !o.Acknowledged {
				acknowledged = false
			}
		}
		if acknowledged {
			return ops
		}
		if time.Now().After(deadline) {
			t.Fatalf("operations were not acknowledged: %+v", ops)
		}
		time.Sleep(20 * time.Millisecond)
	}
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
	deadline := time.Now().Add(DemoTimeout)
	for {
		resp, err := c.Session(context.Background(), id)
		Must(t, err)
		if done(resp.Session) {
			return resp.Session
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not settle: %+v", resp.Session)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
