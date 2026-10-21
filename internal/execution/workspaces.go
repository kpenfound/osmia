package execution

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/workspace"
)

// checkpointed runs one local step and its durable progress write. External
// calls belong before or after this span, using the original context. The
// callback uses the supplied context to share the clone lock with workspace
// methods. An interrupted step is recovered before another step can run.
func checkpointed(ctx context.Context, operation string, at time.Time, providers []workspace.Provider, step func(context.Context) (coreadapter.OperationResult, error)) (result coreadapter.OperationResult, err error) {
	err = workspace.LocalStep(ctx, operation, at, providers, func(local context.Context) error {
		var stepErr error
		result, stepErr = step(local)
		return stepErr
	})
	return result, err
}

// captureFiles copies a durable view back and snapshots its files under the
// same local lock. An interrupted copy is retried from that surviving view.
func captureFiles(ctx context.Context, g workspace.Provider, source, path string) error {
	return workspace.LocalStep(ctx, "capture/"+source, time.Now().UTC(), []workspace.Provider{g}, func(local context.Context) error {
		if err := mirror(source, path); err != nil {
			return err
		}
		_, err := g.Record(local, workspace.Worktree{Path: path})
		return err
	})
}
