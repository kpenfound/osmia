package service

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/kpenfound/osmia/internal/runtime"
)

// archiveCleanup also migrates previously archived workstreams: the saved
// title precedes deletion, and CleanedAt makes completion survive restarts.
type archiveCleanup struct {
	s          *Service
	workspaces *workspaceCleanup
}

func (c *archiveCleanup) Pass(ctx context.Context) error {
	state, _ := c.s.store.Snapshot()
	for _, a := range state.Archived {
		if a.Project != c.workspaces.cfg.Project.ID || !a.CleanedAt.IsZero() {
			continue
		}
		if err := c.workspaces.attempt(ctx, "archive/"+string(a.Workstream), func() error { return c.clean(ctx, a) }); err != nil {
			return err
		}
	}
	return nil
}

func (c *archiveCleanup) clean(ctx context.Context, a runtime.Archive) error {
	repo := c.workspaces.repository
	streams, err := repo.Workstreams()
	if err != nil {
		return err
	}
	if slices.Contains(streams, a.Workstream) {
		if a.Title == "" {
			metadata, err := archiveTitle(repo, a.Workstream)
			if err != nil {
				return err
			}
			a.Title, a.State = metadata.Title, metadata.State
			if a.State != DeliveredState && a.State != AbandonedState {
				return fmt.Errorf("archived workstream is not terminal")
			}
			if err := c.s.store.UpdateArchive(a); err != nil {
				return err
			}
		}
		busy, chief, err := c.workspaces.busy(a.Workstream)
		if err != nil || len(busy) != 0 || chief {
			return err
		}
		if pending, err := c.workspaces.pending(a.Workstream); err != nil || pending {
			return err
		}
		operations, err := repo.Operations(a.Workstream)
		if err != nil {
			return err
		}
		for _, op := range operations {
			if !op.Acknowledged {
				return nil
			}
		}
		// A retained dependent still needs the parent's seal and publication
		// records. Purge dependents first; the next pass can then purge the base.
		for _, stream := range streams {
			if stream == a.Workstream {
				continue
			}
			base, err := repo.WorkstreamBase(stream)
			if err != nil {
				return err
			}
			if base.Base == a.Workstream {
				return nil
			}
		}
		if err := c.workspaces.clean(ctx, a.Workstream); err != nil {
			return err
		}
		for _, dir := range append([]string{unitsDirectory, branchesDirectory, driftsDirectory, reviewInputsDirectory}, turnDirectories...) {
			path := filepath.Join(c.workspaces.cfg.Root.String(), dir, string(a.Project), string(a.Workstream))
			present, err := exists(path)
			if err != nil {
				return err
			}
			if present {
				return nil
			}
		}
		if err := repo.PurgeWorkstream(ctx, a.Workstream); err != nil {
			return err
		}
	}
	if a.Title == "" {
		return fmt.Errorf("archived workstream %s has no trace from which to preserve its title", a.Workstream)
	}
	a.CleanedAt = c.s.now()
	return c.s.store.UpdateArchive(a)
}
