package adaptertest

import (
	"context"
	a "github.com/kpenfound/osmia/internal/coreadapter"
	"sync"
)

type Turns struct {
	Script[a.PreparedTurn, a.SessionResult]
}

func (f *Turns) Run(ctx context.Context, req a.PreparedTurn) (a.SessionResult, error) {
	return f.Call(ctx, req)
}

var _ a.Turns = (*Turns)(nil)

type Workspaces struct {
	Script[a.WorkspaceRequest, a.WorkspaceLease]
}

func (f *Workspaces) Acquire(ctx context.Context, req a.WorkspaceRequest) (a.WorkspaceLease, error) {
	return f.Call(ctx, req)
}

var _ a.Workspaces = (*Workspaces)(nil)

// Lease records cleanup attempts. After a successful release subsequent calls
// succeed without consuming a reply; failures can be retried with a fresh context.
type Lease struct {
	mu       sync.Mutex
	released bool
	Releases Script[struct{}, struct{}]
}

func (f *Lease) Release(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.released {
		return nil
	}
	_, err := f.Releases.Call(ctx, struct{}{})
	if err == nil {
		f.released = true
	}
	return err
}

var _ a.Lease = (*Lease)(nil)
