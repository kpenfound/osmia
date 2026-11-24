package execution

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/workspace"
)

func checkpointed(ctx context.Context, operation string, at time.Time, providers []workspace.Provider, step func(context.Context) (coreadapter.OperationResult, error)) (result coreadapter.OperationResult, err error) {
	err = workspace.LocalStep(ctx, operation, at, providers, func(local context.Context) error {
		var stepErr error
		result, stepErr = step(local)
		return stepErr
	})
	return result, err
}

func captureFiles(ctx context.Context, g workspace.Provider, source, path string) error {
	return workspace.LocalStep(ctx, "capture/"+source, time.Now().UTC(), []workspace.Provider{g}, func(local context.Context) error {
		if err := Mirror(source, path); err != nil {
			return err
		}
		_, err := g.Record(local, workspace.Worktree{Path: path})
		return err
	})
}
