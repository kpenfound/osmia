package trace

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
)

// MarkWorkstreamTraceForDeletion durably marks stream's own dedicated trace
// for permanent deletion: any cached open instance's database is closed and
// the instance evicted so its flock is released, then the trace directory is
// moved, by one
// atomic rename, out of WorkstreamTraces and into WorkstreamDeleting. The
// rename alone makes deletion restart-safe: once it lands, the workstream is
// gone from WorkstreamTraces and so never again discoverable as live, and a
// crash before the actual removal finishes is resumed by
// FinishWorkstreamTraceDeletion, called later with the same arguments. It
// reports moved=false, with no error, when the trace is already gone
// from WorkstreamTraces, whether because an earlier call already moved or
// removed it, or because it never existed. Must be called on the project's
// own instance, never a workstream's.
func (r *Repository) MarkWorkstreamTraceForDeletion(stream config.WorkstreamID) (bool, error) {
	return r.markWorkstreamTraceForDeletion(stream, nil)
}

func (r *Repository) markWorkstreamTraceForDeletion(stream config.WorkstreamID, beforeClose func()) (bool, error) {
	if r.stream != "" {
		return false, fmt.Errorf("a workstream trace instance marks no workstream trace of its own for deletion")
	}
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return false, err
	}
	r.snapshotMu.Lock()
	defer r.snapshotMu.Unlock()
	r.workstreamsMu.Lock()
	defer r.workstreamsMu.Unlock()
	// Scoped opens must see either the cached live handle or the missing
	// directory, never an evicted handle whose flock has not been released.
	child := r.workstreams[stream]
	delete(r.workstreams, stream)
	if beforeClose != nil {
		beforeClose()
	}
	if child != nil {
		if err := child.closeFiles(); err != nil {
			return false, err
		}
	}
	source, err := r.root.WorkstreamTrace(r.project, stream)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(source); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	deleting, err := r.root.WorkstreamDeleting(r.project)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(deleting, 0700); err != nil {
		return false, err
	}
	if err := os.Rename(source, filepath.Join(deleting, string(stream))); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// FinishWorkstreamTraceDeletion removes stream's trace directory from
// WorkstreamDeleting, where MarkWorkstreamTraceForDeletion, or an earlier
// call to this function interrupted before it finished, left it. It touches
// no trace lock or database, so it may take as long as removing the
// files does without delaying a read, a write or reconciliation of any
// workstream. It is a no-op when nothing is there.
func FinishWorkstreamTraceDeletion(root config.Root, project config.ProjectID, stream config.WorkstreamID) error {
	deleting, err := root.WorkstreamDeleting(project)
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(deleting, string(stream)))
}

// WorkstreamTraceDeletionPending reports whether stream's trace is present
// in WorkstreamDeleting, left by MarkWorkstreamTraceForDeletion or an
// earlier, interrupted call to FinishWorkstreamTraceDeletion, and so needs
// FinishWorkstreamTraceDeletion run, or run again, before the trace is gone.
func WorkstreamTraceDeletionPending(root config.Root, project config.ProjectID, stream config.WorkstreamID) (bool, error) {
	deleting, err := root.WorkstreamDeleting(project)
	if err != nil {
		return false, err
	}
	_, err = os.Lstat(filepath.Join(deleting, string(stream)))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// WorkstreamTraceGone reports whether stream's trace directory is fully
// gone: absent from WorkstreamTraces, where it would be live, and absent
// from WorkstreamDeleting, where a deletion in progress or interrupted
// would leave it. Only then may a caller treat the trace as permanently
// deleted.
func WorkstreamTraceGone(root config.Root, project config.ProjectID, stream config.WorkstreamID) (bool, error) {
	live, err := root.WorkstreamTrace(project, stream)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(live); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	pending, err := WorkstreamTraceDeletionPending(root, project, stream)
	return !pending, err
}
