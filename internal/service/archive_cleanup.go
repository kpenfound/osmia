package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// archiveCleanup permanently deletes the independent storage of a project's
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
type archiveCleanup struct {
	s          *Service
	workspaces *workspaceCleanup

	mu sync.Mutex
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
	finish func(config.Root, config.ProjectID, config.WorkstreamID) error
}

func (c *archiveCleanup) Pass(ctx context.Context) error {
	project := c.workspaces.cfg.Project.ID
	state, _ := c.s.store.Snapshot()
	ready := map[config.WorkstreamID]bool{}
	reasons := map[config.WorkstreamID]string{}
	for _, a := range state.Archived {
		if a.Project != project || !a.CleanedAt.IsZero() {
			continue
		}
		if err := c.workspaces.attempt(ctx, "archive/"+string(a.Workstream), func() error {
			eligible, reason, err := c.prepare(ctx, a)
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
		if err := c.workspaces.attempt(ctx, "archive-batch", func() error { return c.resolveDependencies(ready, reasons) }); err != nil {
			return err
		}
	}
	for stream, reason := range reasons {
		c.logDeferral(stream, reason)
	}
	for stream := range ready {
		c.logDeferral(stream, "")
		if err := c.beginDeletion(stream); err != nil {
			return err
		}
	}
	state, _ = c.s.store.Snapshot()
	return c.reconcile(state.Archived)
}

// prepare reports whether a's workstream is ready to have its trace
// deleted. While the trace is still present it also durably fills a
// pre-existing archive record's title, outcome and delivery link, so an
// archive from before this feature carries them too. It returns a
// non-empty reason exactly when eligible is false and err is nil.
func (c *archiveCleanup) prepare(ctx context.Context, a runtime.Archive) (eligible bool, reason string, err error) {
	repo := c.workspaces.repository
	streams, err := repo.Workstreams()
	if err != nil {
		return false, "", err
	}
	if !slices.Contains(streams, a.Workstream) {
		if a.Title == "" {
			return false, "", fmt.Errorf("archived workstream %s has no trace from which to preserve its title", a.Workstream)
		}
		return true, "", nil
	}
	if a.Title == "" {
		metadata, err := archiveTitle(repo, a.Workstream)
		if err != nil {
			return false, "", err
		}
		a.Title, a.State = metadata.Title, metadata.State
		if a.State != DeliveredState && a.State != AbandonedState {
			return false, "", fmt.Errorf("archived workstream is not terminal")
		}
		if err := c.s.store.UpdateArchive(a); err != nil {
			return false, "", err
		}
	}
	if a.DeliveryLink == "" {
		link, err := deliveryLinkFor(repo, a.Workstream)
		if err != nil {
			return false, "", err
		}
		if link != "" {
			a.DeliveryLink = link
			if err := c.s.store.UpdateArchive(a); err != nil {
				return false, "", err
			}
		}
	}
	busy, chief, err := c.workspaces.busy(a.Workstream)
	if err != nil {
		return false, "", err
	}
	if len(busy) != 0 || chief {
		return false, "the workstream has an unfinished turn", nil
	}
	if pending, err := c.workspaces.pending(a.Workstream); err != nil {
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
	if err := c.workspaces.clean(ctx, a.Workstream); err != nil {
		return false, "", err
	}
	for _, dir := range append([]string{unitsDirectory, branchesDirectory, driftsDirectory, reviewInputsDirectory}, turnDirectories...) {
		path := filepath.Join(c.workspaces.cfg.Root.String(), dir, string(a.Project), string(a.Workstream))
		present, err := exists(path)
		if err != nil {
			return false, "", err
		}
		if present {
			return false, "a temporary workspace is still being cleaned", nil
		}
	}
	if incorporated, err := learningsIncorporated(repo, a.Workstream); err != nil {
		return false, "", err
	} else if !incorporated {
		return false, "the librarian has not yet incorporated the workstream's learnings into project knowledge", nil
	}
	return true, "", nil
}

// resolveDependencies removes from ready any base a retained workstream
// still depends on, recording the reason for it in reasons. A base may
// leave with its dependents, so this repeats: retaining a dependent can
// retain its ancestors as well.
func (c *archiveCleanup) resolveDependencies(ready map[config.WorkstreamID]bool, reasons map[config.WorkstreamID]string) error {
	repo := c.workspaces.repository
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

// beginDeletion durably marks stream's trace repository for deletion, then
// hands its actual removal to a background goroutine so this pass, and the
// lock it runs serialized under, never waits for it.
func (c *archiveCleanup) beginDeletion(stream config.WorkstreamID) error {
	moved, err := c.workspaces.repository.MarkWorkstreamTraceForDeletion(stream)
	if err != nil {
		return err
	}
	if moved {
		c.s.log().Info(fmt.Sprintf("archive cleanup is deleting the trace of workstream %s of project %s", stream, c.workspaces.cfg.Project.ID), "component", "archive_cleanup", "project", string(c.workspaces.cfg.Project.ID), "workstream", string(stream))
	}
	c.startRemoval(stream)
	return nil
}

// startRemoval launches, at most once at a time per workstream, a
// goroutine that finishes removing stream's moved-aside trace. It holds no
// repository lock, so a large trace's removal never delays a read, a write
// or reconciliation of any workstream.
func (c *archiveCleanup) startRemoval(stream config.WorkstreamID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.removing == nil {
		c.removing = map[config.WorkstreamID]bool{}
	}
	if c.removing[stream] {
		return
	}
	c.removing[stream] = true
	finish := c.finish
	if finish == nil {
		finish = trace.FinishWorkstreamTraceDeletion
	}
	root, project := c.workspaces.cfg.Root, c.workspaces.cfg.Project.ID
	go func() {
		err := finish(root, project, stream)
		c.mu.Lock()
		delete(c.removing, stream)
		c.mu.Unlock()
		if err != nil {
			c.s.log().Warn(fmt.Sprintf("archive cleanup could not remove the trace of workstream %s of project %s: %v; it will retry", stream, project, err), "component", "archive_cleanup", "project", string(project), "workstream", string(stream), "error", err.Error())
			return
		}
		c.s.log().Info(fmt.Sprintf("archive cleanup removed the trace of workstream %s of project %s", stream, project), "component", "archive_cleanup", "project", string(project), "workstream", string(stream))
	}()
}

// reconcile marks each not-yet-clean archive of this project clean once
// its trace is confirmed fully gone, from its usual location and from
// where a deletion in progress would leave it, and resumes, if nothing is
// already removing it, any deletion left incomplete by an earlier pass or
// a crash. A half-deleted trace is therefore never left for long, and
// never mistaken for a live one.
func (c *archiveCleanup) reconcile(archived []runtime.Archive) error {
	project := c.workspaces.cfg.Project.ID
	root := c.workspaces.cfg.Root
	for _, a := range archived {
		if a.Project != project || !a.CleanedAt.IsZero() {
			continue
		}
		gone, err := trace.WorkstreamTraceGone(root, project, a.Workstream)
		if err != nil {
			return err
		}
		if gone {
			a.CleanedAt = c.s.now()
			if err := c.s.store.UpdateArchive(a); err != nil {
				return err
			}
			c.logDeferral(a.Workstream, "")
			c.s.log().Info(fmt.Sprintf("archive cleanup completed for workstream %s of project %s", a.Workstream, project), "component", "archive_cleanup", "project", string(project), "workstream", string(a.Workstream))
			continue
		}
		pending, err := trace.WorkstreamTraceDeletionPending(root, project, a.Workstream)
		if err != nil {
			return err
		}
		if pending {
			c.startRemoval(a.Workstream)
		}
	}
	return nil
}

// logDeferral logs reason for workstream the first time it differs from
// what this instance last logged for it, so a steady deferral is logged
// once rather than every pass. An empty reason clears what was logged,
// without logging anything itself.
func (c *archiveCleanup) logDeferral(stream config.WorkstreamID, reason string) {
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
	c.s.log().Info(fmt.Sprintf("archive cleanup of workstream %s of project %s deferred: %s", stream, c.workspaces.cfg.Project.ID, reason), "component", "archive_cleanup", "project", string(c.workspaces.cfg.Project.ID), "workstream", string(stream), "reason", reason)
}

// learningsIncorporated reports whether the librarian refresh covering
// stream's landed units, and any knowledge gap behind a code-cited answer
// on it, has completed. Lasting project-scope decisions reach project
// knowledge synchronously when they are ruled, so nothing further is
// required of them here. A project with no librarian workstream has
// nothing to incorporate.
func learningsIncorporated(repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	librarian := librarianWorkstream(repo.Project())
	streams, err := repo.Workstreams()
	if err != nil {
		return false, err
	}
	if !slices.Contains(streams, librarian) {
		return true, nil
	}
	ops, err := repo.Operations(librarian)
	if err != nil {
		return false, err
	}
	known := map[string]bool{}
	for _, op := range ops {
		if op.Operation.Action != RefreshAction {
			continue
		}
		var in refreshInput
		if err := json.Unmarshal(op.Operation.Input, &in); err != nil {
			return false, err
		}
		if op.Result == nil {
			if in.Workstream == stream {
				return false, nil
			}
			continue
		}
		for _, l := range in.landings() {
			known[l.Commit] = true
		}
		known[in.key()] = true
	}
	r := &refresher{extractor: &extractor{repository: repo}}
	landings, err := r.unfolded(stream, known)
	if err != nil {
		return false, err
	}
	if len(landings) != 0 {
		return false, nil
	}
	rulings, err := trace.Read[trace.Ruling](repo, stream)
	if err != nil {
		return false, err
	}
	for _, ruling := range rulings {
		if ruling.Decision != trace.DecisionAnswer {
			continue
		}
		for _, citation := range ruling.Citations {
			inspectionID, ok := strings.CutPrefix(citation, "inspection#")
			if !ok {
				continue
			}
			inspection, err := repo.CodeInspection(stream, inspectionID)
			if err != nil {
				return false, err
			}
			in := refreshInput{Workstream: stream, Question: ruling.QuestionID, Inspection: inspectionID, Commit: inspection.Commit}
			if !known[in.key()] {
				return false, nil
			}
		}
	}
	return true, nil
}
