package service

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
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
	ready := map[config.WorkstreamID]runtime.Archive{}
	for _, a := range state.Archived {
		if a.Project != c.workspaces.cfg.Project.ID || !a.CleanedAt.IsZero() {
			continue
		}
		if err := c.workspaces.attempt(ctx, "archive/"+string(a.Workstream), func() error {
			prepared, eligible, err := c.prepare(ctx, a)
			if err == nil && eligible {
				ready[a.Workstream] = prepared
			}
			return err
		}); err != nil {
			return err
		}
	}
	if len(ready) == 0 {
		return nil
	}
	return c.workspaces.attempt(ctx, "archive-batch", func() error {
		repo := c.workspaces.repository
		streams, err := repo.Workstreams()
		if err != nil {
			return err
		}
		// A base can leave with its dependents, but never while one is retained.
		// Repeat because retaining a dependent can retain its ancestors as well.
		bases := map[config.WorkstreamID]config.WorkstreamID{}
		for _, stream := range streams {
			base, err := repo.WorkstreamBase(stream)
			if err != nil {
				return err
			}
			bases[stream] = base.Base
		}
		for changed := true; changed; {
			changed = false
			for stream, base := range bases {
				if _, selected := ready[stream]; selected {
					continue
				}
				if _, selected := ready[base]; selected {
					delete(ready, base)
					changed = true
				}
			}
		}
		if len(ready) == 0 {
			return nil
		}
		batch := make([]config.WorkstreamID, 0, len(ready))
		for stream := range ready {
			batch = append(batch, stream)
		}
		slices.Sort(batch)
		if err := repo.PurgeWorkstreams(ctx, batch); err != nil {
			return err
		}
		for _, stream := range batch {
			a := ready[stream]
			a.CleanedAt = c.s.now()
			if err := c.s.store.UpdateArchive(a); err != nil {
				return err
			}
		}
		return nil
	})
}

// prepare preserves the title and releases temporary workspaces before a
// terminal, idle archive is considered for the project's deletion batch.
func (c *archiveCleanup) prepare(ctx context.Context, a runtime.Archive) (runtime.Archive, bool, error) {
	repo := c.workspaces.repository
	streams, err := repo.Workstreams()
	if err != nil {
		return a, false, err
	}
	if slices.Contains(streams, a.Workstream) {
		if a.Title == "" {
			metadata, err := archiveTitle(repo, a.Workstream)
			if err != nil {
				return a, false, err
			}
			a.Title, a.State = metadata.Title, metadata.State
			if a.State != DeliveredState && a.State != AbandonedState {
				return a, false, fmt.Errorf("archived workstream is not terminal")
			}
			if err := c.s.store.UpdateArchive(a); err != nil {
				return a, false, err
			}
		}
		busy, chief, err := c.workspaces.busy(a.Workstream)
		if err != nil || len(busy) != 0 || chief {
			return a, false, err
		}
		if pending, err := c.workspaces.pending(a.Workstream); err != nil || pending {
			return a, false, err
		}
		operations, err := repo.Operations(a.Workstream)
		if err != nil {
			return a, false, err
		}
		for _, op := range operations {
			if !op.Acknowledged {
				return a, false, nil
			}
		}
		if err := c.workspaces.clean(ctx, a.Workstream); err != nil {
			return a, false, err
		}
		for _, dir := range append([]string{unitsDirectory, branchesDirectory, driftsDirectory, reviewInputsDirectory}, turnDirectories...) {
			path := filepath.Join(c.workspaces.cfg.Root.String(), dir, string(a.Project), string(a.Workstream))
			present, err := exists(path)
			if err != nil {
				return a, false, err
			}
			if present {
				return a, false, nil
			}
		}
	}
	if a.Title == "" {
		return a, false, fmt.Errorf("archived workstream %s has no trace from which to preserve its title", a.Workstream)
	}
	return a, true, nil
}
