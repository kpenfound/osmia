package coreadapter

import (
	"context"
	"errors"
	"sync"

	"github.com/kpenfound/busybees/core/vcs"
)

// releaseLease serializes release and permits retry after a failed cleanup.
type releaseLease struct {
	mu       sync.Mutex
	released bool
	release  func(context.Context) error
}

func (l *releaseLease) Release(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	if err := l.release(ctx); err != nil {
		return err
	}
	l.released = true
	return nil
}

// WorkspaceAdapter delegates provider selection and naming to the service.
// Select must reject unsupported source, revision, access or directory requests;
// core's Provider interface itself supplies no read-only enforcement guarantee.
type WorkspaceAdapter struct {
	Select func(context.Context, WorkspaceRequest) (vcs.Provider, vcs.Request, error)
}

var _ Workspaces = (*WorkspaceAdapter)(nil)

func (a *WorkspaceAdapter) Acquire(ctx context.Context, req WorkspaceRequest) (WorkspaceLease, error) {
	if err := ctx.Err(); err != nil {
		return WorkspaceLease{}, err
	}
	if req.Access != ReadOnly && req.Access != ReadWrite {
		return WorkspaceLease{}, unsupported("workspace access", string(req.Access))
	}
	if a.Select == nil {
		return WorkspaceLease{}, unsupported("workspace provider", "no provider selector supplied")
	}
	provider, coreReq, err := a.Select(ctx, req)
	if err != nil {
		return WorkspaceLease{}, err
	}
	if provider == nil {
		return WorkspaceLease{}, unsupported("workspace provider", "selector returned no provider")
	}
	workspace, err := provider.Acquire(ctx, coreReq)
	if workspace == nil {
		if err == nil {
			err = errors.New("provider returned no workspace")
		}
		return WorkspaceLease{}, err
	}
	lease := &releaseLease{release: func(ctx context.Context) error { return provider.Release(ctx, workspace) }}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && (workspace.Directory() == "" || (req.Directory != "" && req.Directory != workspace.Directory())) {
		err = errors.New("provider workspace directory does not match request")
	}
	if err != nil {
		return WorkspaceLease{}, errors.Join(err, lease.Release(context.WithoutCancel(ctx)))
	}
	return WorkspaceLease{Workspace: Workspace{ID: coreReq.Name, Directory: workspace.Directory(), Revision: req.BaseRevision, Access: req.Access}, Lease: lease}, nil
}
