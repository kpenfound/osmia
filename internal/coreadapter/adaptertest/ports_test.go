package adaptertest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	a "github.com/kpenfound/osmia/internal/coreadapter"
)

func exercise[Q, R any](t *testing.T, s *Script[Q, R], call func(context.Context, Q) (R, error), req Q, result R) {
	t.Helper()
	failure := errors.New("provider unavailable")
	*s = *NewScript[Q, R](Reply[R]{Value: result}, Reply[R]{Value: result, Err: a.ErrUnsupported}, Reply[R]{Value: result, Err: failure})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := call(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if len(s.Calls()) != 0 {
		t.Fatal("cancelled call consumed script")
	}
	for _, wantErr := range []error{nil, a.ErrUnsupported, failure} {
		got, err := call(context.Background(), req)
		if !errors.Is(err, wantErr) || !reflect.DeepEqual(got, result) {
			t.Fatalf("got %#v, %v; want %#v, %v", got, err, result, wantErr)
		}
	}
	if got := s.Calls(); !reflect.DeepEqual(got, []Q{req, req, req}) {
		t.Fatalf("requests lost: %#v", got)
	}
	if _, err := call(context.Background(), req); !errors.Is(err, ErrExhausted) {
		t.Fatalf("exhaustion: %v", err)
	}
}
func TestPorts(t *testing.T) {
	t.Run("Turns", func(t *testing.T) {
		f := &Turns{}
		var port a.Turns = f
		exercise(t, &f.Script, port.Run, a.PreparedTurn{Prompt: "build", Scope: a.Scope{Thread: "thread", Turn: "turn"}, Resume: &a.BackendSession{Backend: "fake", ID: "resume"}}, a.SessionResult{FinalResponse: "done", Outcome: &a.Outcome{Status: "waiting"}, Usage: a.Usage{CostKnown: false}})
	})
	t.Run("Workspaces", func(t *testing.T) {
		f := &Workspaces{}
		var port a.Workspaces = f
		exercise(t, &f.Script, port.Acquire, a.WorkspaceRequest{Directory: "/files", Access: a.ReadOnly}, a.WorkspaceLease{Workspace: a.Workspace{Directory: "/files", Access: a.ReadOnly}})
	})
}
