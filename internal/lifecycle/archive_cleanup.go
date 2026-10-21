package lifecycle

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// ArchiveCleanup permanently deletes the independent storage of a project's
// terminal, idle archived workstreams: their own dedicated trace
// repositories and their temporary workspaces. It never rewrites or prunes
// Git history; it only deletes files. It also migrates archive records
// kept from before this feature, filling their title, outcome and delivery
// link while the trace is still present.
//
// Deletion of a workstream's trace is deferred, with a logged reason,
// while it has an unfinished turn, a pending or unacknowledged operation,
// or a temporary workspace still being cleaned; while a retained
// workstream still depends on it as a base; or until the librarian refresh
// covering its landed units and any knowledge gap it filed has completed.
// Once every deferral clears, the trace repository is moved aside by one
// atomic rename -- durable, restart-safe intent to delete it, after which
// it is never again discoverable as live -- and its actual removal runs in
// a background goroutine that holds no repository lock, so deleting even a
// large trace never delays a read, a write or reconciliation of any other
// workstream. The archive record is marked clean only once the storage is
// confirmed gone, which a later pass reconciles whether an earlier attempt
// was interrupted by a crash or is simply still in flight.
type ArchiveCleanup struct {
	S          *Controller
	Workspaces *WorkspaceCleanup
	mu         sync.Mutex
	// removing tracks, by workstream, a deletion a background goroutine of
	// this instance is already carrying out, so a later pass does not start
	// a second one for the same deletion.
	removing map[config.WorkstreamID]bool
	// deferred is the reason this instance last logged for not deleting a
	// workstream's trace, by workstream, so a steady deferral is logged
	// once, not every pass.
	deferred map[config.WorkstreamID]string
	// finish performs a workstream's actual, possibly slow, trace removal.
	// It is trace.FinishWorkstreamTraceDeletion unless a test replaces it to
	// observe or hold a deletion in flight.
	Finish func(config.Root, config.ProjectID, config.WorkstreamID) error
}

// Pass reports progress when the project has archives awaiting cleanup.
// Idle passes leave the activity record and event stream quiet.
func (c *ArchiveCleanup) Pass(ctx context.Context) error {
	project := c.Workspaces.Cfg.Project.ID
	state, _ := c.S.Store().Snapshot()
	if !slices.ContainsFunc(state.Archived, func(a runtime.Archive) bool {
		return a.Project == project && a.CleanedAt.IsZero()
	}) {
		return nil
	}
	h := c.S.StartActivity("archive-cleanup:"+string(project), ActivityArchiveCleanup, project, "", "scanning archived workstreams")
	err := c.RunPass(ctx, h)
	h.Finish(err)
	return err
}
func (c *ArchiveCleanup) RunPass(ctx context.Context, h *ActivityHandle) error {
	project := c.Workspaces.Cfg.Project.ID
	state, _ := c.S.Store().Snapshot()
	ready := map[config.WorkstreamID]bool{}
	reasons := map[config.WorkstreamID]string{}
	for _, a := range state.Archived {
		if a.Project != project || !a.CleanedAt.IsZero() {
			continue
		}
		h.Progress("preparing "+string(a.Workstream), nil)
		if err := c.Workspaces.Attempt(ctx, "archive/"+string(a.Workstream), func() error {
			eligible, reason, err := c.Prepare(ctx, a)
			if err != nil {
				return err
			}
			if eligible {
				ready[a.Workstream] = true
			} else {
				reasons[a.Workstream] = reason
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if len(ready) != 0 {
		if err := c.Workspaces.Attempt(ctx, "archive-batch", func() error {
			return c.ResolveDependencies(ready, reasons)
		}); err != nil {
			return err
		}
	}
	for stream, reason := range reasons {
		c.LogDeferral(stream, reason)
	}
	if len(ready) != 0 {
		h.Progress(fmt.Sprintf("deleting %d workstream traces", len(ready)), nil)
	}
	for stream := range ready {
		c.LogDeferral(stream, "")
		if err := c.BeginDeletion(stream); err != nil {
			return err
		}
	}
	state, _ = c.S.Store().Snapshot()
	return c.Reconcile(state.Archived)
}

// Prepare reports whether a's workstream is ready to have its trace
// deleted. While the trace is still present it also durably fills a
// pre-existing archive record's title, outcome and delivery link, so an
// Archive from before this feature carries them too. It returns a
// non-empty reason exactly when eligible is false and err is nil.
func (c *ArchiveCleanup) Prepare(ctx context.Context, a runtime.Archive) (eligible bool, reason string, err error) {
	repo := c.Workspaces.Repository
	streams, err := repo.Workstreams()
	if err != nil {
		return false, "", err
	}
	if !slices.Contains(streams, a.Workstream) {
		if a.Title == "" {
			return false, "", fmt.Errorf("archived workstream %s has no trace from which to preserve its title", a.Workstream)
		}
		if a.CleanupStartedAt.IsZero() {
			return false, "archived trace is missing without recorded cleanup intent", nil
		}
		return true, "", nil
	}
	if a.Title == "" {
		metadata, err := ArchiveTitle(repo, a.Workstream)
		if err != nil {
			return false, "", err
		}
		a.Title, a.State = metadata.Title, metadata.State
		if a.State != DeliveredState && a.State != AbandonedState {
			return false, "", fmt.Errorf("archived workstream is not terminal")
		}
		if err := c.S.Store().UpdateArchive(a); err != nil {
			return false, "", err
		}
	}
	if a.DeliveryLink == "" {
		link, err := DeliveryLinkFor(repo, a.Workstream)
		if err != nil {
			return false, "", err
		}
		if link != "" {
			a.DeliveryLink = link
			if err := c.S.Store().UpdateArchive(a); err != nil {
				return false, "", err
			}
		}
	}
	if err := c.S.ArchiveLifecycle(repo, &a); err != nil {
		return false, "", err
	}
	if err := c.S.Store().UpdateArchive(a); err != nil {
		return false, "", err
	}
	busy, chief, err := c.Workspaces.Busy(a.Workstream)
	if err != nil {
		return false, "", err
	}
	if len(busy) != 0 || chief {
		return false, "the workstream has an unfinished turn", nil
	}
	if pending, err := c.Workspaces.Pending(a.Workstream); err != nil {
		return false, "", err
	} else if pending {
		return false, "the workstream has an operation without a result", nil
	}
	operations, err := repo.Operations(a.Workstream)
	if err != nil {
		return false, "", err
	}
	for _, op := range operations {
		if !op.Acknowledged {
			return false, "the workstream has an unacknowledged operation", nil
		}
	}
	if err := c.Workspaces.Clean(ctx, a.Workstream); err != nil {
		return false, "", err
	}
	for _, dir := range append([]string{unitsDirectory, branchesDirectory, driftsDirectory, ReviewInputsDirectory}, TurnDirectories...) {
		path := filepath.Join(c.Workspaces.Cfg.Root.String(), dir, string(a.Project), string(a.Workstream))
		present, err := Exists(path)
		if err != nil {
			return false, "", err
		}
		if present {
			return false, "a temporary workspace is still being cleaned", nil
		}
	}
	if incorporated, err := c.S.LearningsIncorporated(repo, a.Workstream); err != nil {
		return false, "", err
	} else if !incorporated {
		return false, "the librarian has not yet incorporated the workstream's learnings into project knowledge", nil
	}
	return true, "", nil
}

// ResolveDependencies removes from ready any base a retained workstream
// still depends on, recording the reason for it in reasons. A base may
// leave with its dependents, so this repeats: retaining a dependent can
// retain its ancestors as well.
func (c *ArchiveCleanup) ResolveDependencies(ready map[config.WorkstreamID]bool, reasons map[config.WorkstreamID]string) error {
	repo := c.Workspaces.Repository
	streams, err := repo.Workstreams()
	if err != nil {
		return err
	}
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
			if ready[stream] {
				continue
			}
			if ready[base] {
				delete(ready, base)
				reasons[base] = "a retained workstream depends on it as a base"
				changed = true
			}
		}
	}
	return nil
}

// BeginDeletion durably marks stream's trace repository for deletion, then
// hands its actual removal to a background goroutine so this pass, and the
// lock it runs serialized under, never waits for it.
func (c *ArchiveCleanup) BeginDeletion(stream config.WorkstreamID) error {
	if err := c.S.Step("archive-cleanup-ready"); err != nil {
		return err
	}
	state, _ := c.S.Store().Snapshot()
	authorized := false
	for _, a := range state.Archived {
		if a.Project == c.Workspaces.Repository.Project() && a.Workstream == stream {
			authorized = true
			if a.CleanupStartedAt.IsZero() {
				a.CleanupStartedAt = c.S.Now()
				if err := c.S.Store().UpdateArchive(a); err != nil {
					return err
				}
			}
		}
	}
	if !authorized {
		return fmt.Errorf("workstream %s has no owner archive record", stream)
	}
	if err := c.S.Step("archive-cleanup-intent"); err != nil {
		return err
	}
	moved, err := c.Workspaces.Repository.MarkWorkstreamTraceForDeletion(stream)
	if err != nil {
		return err
	}
	if moved {
		c.S.Log().Info(fmt.Sprintf("archive cleanup is deleting the trace of workstream %s of project %s", stream, c.Workspaces.Cfg.Project.ID), "component", "archive_cleanup", "project", string(c.Workspaces.Cfg.Project.ID), "workstream", string(stream))
	}
	if err := c.S.Step("archive-cleanup-renamed"); err != nil {
		return err
	}
	c.StartRemoval(stream)
	return nil
}

// StartRemoval launches, at most once at a time per workstream, a
// goroutine that finishes removing stream's moved-aside trace. It holds no
// Repository lock, so a large trace's removal never delays a read, a write
// or reconciliation of any workstream. The removal reports itself in the
// activity record for as long as it runs, and keeps a failure there until
// a later pass retries it.
func (c *ArchiveCleanup) StartRemoval(stream config.WorkstreamID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.removing == nil {
		c.removing = map[config.WorkstreamID]bool{}
	}
	if c.removing[stream] {
		return
	}
	c.removing[stream] = true
	finish := c.Finish
	if finish == nil {
		finish = trace.FinishWorkstreamTraceDeletion
	}
	root, project := c.Workspaces.Cfg.Root, c.Workspaces.Cfg.Project.ID
	h := c.S.StartActivity("archive-cleanup:"+string(project)+"/"+string(stream), ActivityArchiveCleanup, project, stream, "removing the workstream's trace")
	go func() {
		err := finish(root, project, stream)
		h.Finish(err)
		c.mu.Lock()
		delete(c.removing, stream)
		c.mu.Unlock()
		if err != nil {
			c.S.Log().Warn(fmt.Sprintf("archive cleanup could not remove the trace of workstream %s of project %s: %v; it will retry", stream, project, err), "component", "archive_cleanup", "project", string(project), "workstream", string(stream), "error", err.Error())
			return
		}
		c.S.Log().Info(fmt.Sprintf("archive cleanup removed the trace of workstream %s of project %s", stream, project), "component", "archive_cleanup", "project", string(project), "workstream", string(stream))
	}()
}

// Reconcile marks each not-yet-clean archive of this project clean once
// its trace is confirmed fully gone, from its usual location and from
// where a deletion in progress would leave it, and resumes, if nothing is
// already removing it, any deletion left incomplete by an earlier pass or
// a crash. A half-deleted trace is therefore never left for long, and
// never mistaken for a live one.
func (c *ArchiveCleanup) Reconcile(archived []runtime.Archive) error {
	project := c.Workspaces.Cfg.Project.ID
	root := c.Workspaces.Cfg.Root
	for _, a := range archived {
		if a.Project != project || !a.CleanedAt.IsZero() {
			continue
		}
		gone, err := trace.WorkstreamTraceGone(root, project, a.Workstream)
		if err != nil {
			return err
		}
		if gone && !a.CleanupStartedAt.IsZero() {
			a.CleanedAt = c.S.Now()
			if err := c.S.Store().UpdateArchive(a); err != nil {
				return err
			}
			if err := c.S.Step("archive-cleanup-completed"); err != nil {
				return err
			}
			c.LogDeferral(a.Workstream, "")
			c.S.Log().Info(fmt.Sprintf("archive cleanup completed for workstream %s of project %s", a.Workstream, project), "component", "archive_cleanup", "project", string(project), "workstream", string(a.Workstream))
			continue
		}
		pending, err := trace.WorkstreamTraceDeletionPending(root, project, a.Workstream)
		if err != nil {
			return err
		}
		if pending {
			c.StartRemoval(a.Workstream)
		}
	}
	return nil
}

// LogDeferral logs reason for workstream the first time it differs from
// What this instance last logged for it, so a steady deferral is logged
// once rather than every pass. An empty reason clears what was logged,
// without logging anything itself.
func (c *ArchiveCleanup) LogDeferral(stream config.WorkstreamID, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if reason == "" {
		delete(c.deferred, stream)
		return
	}
	if c.deferred == nil {
		c.deferred = map[config.WorkstreamID]string{}
	}
	if c.deferred[stream] == reason {
		return
	}
	c.deferred[stream] = reason
	c.S.Log().Info(fmt.Sprintf("archive cleanup of workstream %s of project %s deferred: %s", stream, c.Workspaces.Cfg.Project.ID, reason), "component", "archive_cleanup", "project", string(c.Workspaces.Cfg.Project.ID), "workstream", string(stream), "reason", reason)
}
