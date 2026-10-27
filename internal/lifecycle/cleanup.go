package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

const (
	// reviewInputsDirectory is the directory under the root holding the
	// exports unit reviewers read, by project, workstream, thread and turn.
	ReviewInputsDirectory = "review-inputs"
	// cleanupRetry is how long a workspace whose removal failed is left
	// before the next attempt.
	CleanupRetry = 5 * time.Minute
)

// TurnDirectories are the directories under the root holding what the turns
// of each workstream ran with, by project and workstream: their session
// directories, with each session's prompts, transcript and result, and the
// views the chief of staff and drift reviewers are staged.
var TurnDirectories = []string{

	"threads", "architect", "shed", "final", "chief_of_staff", "workspaces", "views", "checks"}

// WorkspaceCleanup removes the workspaces of the configured project that the
// workflow is done with: a unit's workspace and its reviewer's export once
// the unit has merged, and every workspace, export and turn directory of a
// delivered or abandoned workstream. The files of a unit that did not merge
// are committed to its branch before its workspace goes, and every branch
// and the trace stay. A unit with a turn unfinished keeps its workspace, and
// a finished workstream keeps the rest while it has an operation pending or
// a turn unfinished. A removal that fails is logged and tried again after
// CleanupRetry.
type WorkspaceCleanup struct {
	Cfg        *config.Config
	Repository *trace.Repository
	Now        func() time.Time
	Failures   map[string]CleanupFailure
}

// CleanupFailure is the last failed removal of one workspace.
type CleanupFailure struct {
	Err   string
	Retry time.Time
}

// Pass removes what every workstream of the project is done with. The trace
// is read only for a workstream one of the project's directories holds
// something of.
func (c *WorkspaceCleanup) Pass(ctx context.Context) error {
	root, project := c.Cfg.Root.String(), string(c.Cfg.Project.ID)
	held := map[string]bool{}
	for _, directory := range append([]string{unitsDirectory, branchesDirectory, driftsDirectory, ReviewInputsDirectory}, TurnDirectories...) {
		names, err := Subdirectories(filepath.Join(root, directory, project))
		if err != nil {
			return err
		}
		for _, name := range names {
			held[name] = true
		}
	}
	if len(held) == 0 {
		return nil
	}
	streams, err := nonSessionWorkstreams(c.Repository)
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if !held[string(stream)] {
			continue
		}
		if err := c.Clean(ctx, stream); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		}
	}
	return nil
}
func (c *WorkspaceCleanup) Clean(ctx context.Context, stream config.WorkstreamID) error {
	root, project := c.Cfg.Root.String(), string(c.Cfg.Project.ID)
	units, err := Subdirectories(filepath.Join(root, unitsDirectory, project, string(stream)))
	if err != nil {
		return err
	}
	exports := filepath.Join(root, ReviewInputsDirectory, project, string(stream))
	feature := filepath.Join(root, branchesDirectory, project, string(stream))
	drift := filepath.Join(root, driftsDirectory, project, string(stream))
	var turns []string
	for _, directory := range TurnDirectories {
		turns = append(turns, filepath.Join(root, directory, project, string(stream)))
	}
	present := map[string]bool{}
	held := len(units) != 0
	for _, dir := range append([]string{exports, feature, drift}, turns...) {
		if present[dir], err = Exists(dir); err != nil {
			return err
		}
		held = held || present[dir]
	}
	if !held {
		return nil
	}
	states, err := c.Repository.WorkflowStates(stream)
	if err != nil {
		return err
	}
	state := states[trace.FeatureSubject].Value
	finished := state == DeliveredState || state == AbandonedState
	if state == DeliveredState {
		waiting, err := EdgeWaitDisplay(c.Repository, stream)
		if err != nil {
			return err
		}
		pending, err := WorkgroupEdgePending(c.Repository, stream)
		if err != nil {
			return err
		}
		finished = waiting == "" && !pending
	}
	var done []string
	for _, unit := range units {
		if finished || states[trace.UnitSubject(unit)].Value == UnitMerged {
			done = append(done, unit)
		}
	}
	if len(done) == 0 && !finished {
		return nil
	}
	busy, chief, err := c.Busy(stream)
	if err != nil {
		return err
	}
	for _, unit := range done {
		if busy[unit] {
			continue
		}
		merged := states[trace.UnitSubject(unit)].Value == UnitMerged
		if err := c.Attempt(ctx, unitsDirectory+"/"+unitName(stream, unit), func() error {
			return c.ReleaseUnit(ctx, stream, unit, merged)
		}); err != nil {
			return err
		}
	}
	if err := c.Attempt(ctx, unitsDirectory+"/"+string(stream), func() error {
		return RemoveEmpty(filepath.Join(root, unitsDirectory, project, string(stream)))
	}); err != nil {
		return err
	}
	if !finished || len(busy) != 0 {
		return nil
	}
	if pending, err := c.Pending(stream); err != nil || pending {
		return err
	}
	if present[exports] {
		if err := c.Attempt(ctx, ReviewInputsDirectory+"/"+string(stream), func() error {
			return os.RemoveAll(exports)
		}); err != nil {
			return err
		}
	}
	for _, kind := range []struct {
		Directory, Dir string
		Workspaces     streamWorkspaces
	}{{branchesDirectory, feature, featureWorkspaces(c.Cfg, c.Repository)}, {driftsDirectory, drift, DriftWorkspaces(c.Cfg, c.Repository)}} {
		if !present[kind.Dir] {
			continue
		}
		if err := c.Attempt(ctx, kind.Directory+"/"+string(stream), func() error {
			return c.Release(ctx, kind.Workspaces, stream)
		}); err != nil {
			return err
		}
	}
	if chief {
		return nil
	}
	for i, dir := range turns {
		if !present[dir] {
			continue
		}
		if err := c.Attempt(ctx, TurnDirectories[i]+"/"+string(stream), func() error {
			return os.RemoveAll(dir)
		}); err != nil {
			return err
		}
	}
	return nil
}

// Busy returns the units of the workstream with a turn unfinished, with the
// empty unit "" when a thread with no unit but the chief of staff's has one,
// and whether the chief of staff has one.
func (c *WorkspaceCleanup) Busy(stream config.WorkstreamID) (map[string]bool, bool, error) {
	threads, err := c.Repository.Threads(stream)
	if err != nil {
		return nil, false, err
	}
	busy, chief := map[string]bool{}, false
	for _, th := range threads {
		unfinished := th.Active != ""
		for _, q := range th.Turns {
			if q.CompletedAt.IsZero() && q.Response == nil {
				unfinished = true
			}
		}
		switch {
		case !unfinished:
		case th.Identity.Role == trace.ChiefOfStaff:
			chief = true
		default:
			busy[th.Identity.Unit] = true
		}
	}
	return busy, chief, nil
}

// Pending reports whether the workstream has an operation without a result.
func (c *WorkspaceCleanup) Pending(stream config.WorkstreamID) (bool, error) {
	operations, err := c.Repository.Operations(stream)
	if err != nil {
		return false, err
	}
	for _, op := range operations {
		if op.Result == nil {
			return true, nil
		}
	}
	return false, nil
}

// ReleaseUnit removes the unit's workspace and its reviewer's export. The
// workspace of a unit that did not merge has its files committed to the
// unit's branch first; one with a replay in progress stays.
func (c *WorkspaceCleanup) ReleaseUnit(ctx context.Context, stream config.WorkstreamID, unit string, merged bool) error {
	g, err := newUnitWorkspaces(c.Cfg, c.Repository).Of(stream)
	if err != nil {
		return err
	}
	w, found, err := g.Workspace(ctx, unitName(stream, unit))
	if err != nil {
		return err
	}
	switch {
	case !found && !merged:
		return errors.New("its directory is no workspace of the clone")
	case !found:
		// The unit's work is on the feature branch, so a directory the clone
		// no longer knows holds nothing to keep.
		err = os.RemoveAll(filepath.Join(c.Cfg.Root.String(), unitsDirectory, string(c.Cfg.Project.ID), string(stream), unit))
	case !merged:
		err = Keep(ctx, g, stream, w)
		if err == nil {
			err = g.Release(ctx, w)
		}
	default:
		err = g.Release(ctx, w)
	}
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(c.Cfg.Root.String(), ReviewInputsDirectory, string(c.Cfg.Project.ID), string(stream), reviewerAgent(unit)))
}

// Keep commits the files of a unit's workspace to the unit's branch.
func Keep(ctx context.Context, g workspace.Provider, stream config.WorkstreamID, w workspace.Worktree) error {
	if _, _, replaying, err := g.Replaying(ctx, w); err != nil {
		return err
	} else if replaying {
		return errors.New("a replay is in progress in it")
	}
	base, err := g.MergeBase(ctx, featureBranch(stream), w.Branch)
	if err != nil {
		return err
	}
	if _, err := g.Snapshot(ctx, w, base); err != nil {
		return fmt.Errorf("commit its files to branch %s: %w", w.Branch, err)
	}
	return nil
}

// Release removes the workstream's workspace of one kind; its branch stays.
func (c *WorkspaceCleanup) Release(ctx context.Context, kind streamWorkspaces, stream config.WorkstreamID) error {
	g, err := kind.Of(stream)
	if err != nil {
		return err
	}
	w, found, err := g.Workspace(ctx, string(stream))
	if err != nil {
		return err
	}
	if !found {
		return errors.New("its directory is no workspace of the clone")
	}
	return g.Release(ctx, w)
}

// Attempt runs one removal unless an earlier failure of it waits for its
// Retry, and logs a failure the first time it differs from the last. It
// returns only the error of a cancelled pass.
func (c *WorkspaceCleanup) Attempt(ctx context.Context, name string, remove func() error) error {
	now := c.Now()
	if f, ok := c.Failures[name]; ok && now.Before(f.Retry) {
		return nil
	}
	err := remove()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		delete(c.Failures, name)
		return nil
	}
	if c.Failures == nil {
		c.Failures = map[string]CleanupFailure{}
	}
	if c.Failures[name].Err != err.Error() {
		ghlog.Logger(ctx).Warn("workspace cleanup retrying", "workspace", name, "project", string(c.Cfg.Project.ID), "error", err.Error(), "retry", CleanupRetry.String())
	}
	c.Failures[name] = CleanupFailure{Err: err.Error(), Retry: now.Add(CleanupRetry)}
	return nil
}

// Subdirectories returns the names of the directories in dir, none when dir
// does not exist.
func Subdirectories(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}
func Exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// RemoveEmpty removes dir when it exists and is empty.
func RemoveEmpty(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || err == nil && len(entries) != 0 {
		return nil
	}
	if err != nil {
		return err
	}
	return os.Remove(dir)
}
