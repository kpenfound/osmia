package activity

import (
	"sort"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
)

// ActivityStallThreshold is how long an activity entry's last progress may go
// unchanged before health reports the backend degraded on its account.
const ActivityStallThreshold = 30 * time.Second

// ActivityFailureExpiry bounds how long a failed entry may degrade health
// and stay listed after nothing has retried it. Many operations key the
// activity record by the trace event that requested them (trace.OperationID),
// not by a retry-stable identity, so a check run superseded by a new run
// number, a turn whose next attempt is a new operation, or a workstream that
// is abandoned after a failure never issues a fresh start under the same key
// to supersede it. Without a bound, that one failure would degrade /health
// forever. Ten minutes comfortably exceeds this project's own automatic
// check retry delay (unit_checks.go, checkRetryDelay), so an operation that
// is actually being retried stays visible through its retry; one that is not
// stops degrading health and drops out of the record shortly after.
const ActivityFailureExpiry = 10 * time.Minute

type activityRecord struct {
	api.ActivityEntry
	reported bool
}

// Registry is the service's in-process record of long-running
// backend operations: archive cleanup, Git and workspace operations,
// reconcile-controller operations, checks, agent turns and startup or
// project recovery. It is separate from the trace and the runtime store, a
// small mutex-guarded map handing out snapshot copies, so reporting and
// reading it never wait on the trace mutex or on operationMu.
//
// Entries are keyed by the caller's own identity for "the same operation",
// such as a reconciled operation's stable ID or a fixed name for a startup
// recovery step. A successful finish removes the entry at once; a failure
// keeps it, recorded, until a fresh start under the same key supersedes it or
// ActivityFailureExpiry passes since the failure, whichever comes first, so
// a repeatedly failing operation never grows the record, a stale failure
// never outlives the operation it reported, and an operation that fails and
// is never retried under the same key does not degrade health forever.
type Registry struct {
	now    func() time.Time
	Notify func()

	mu      sync.Mutex
	entries map[string]*activityRecord
}

func New(now func() time.Time) *Registry {
	return &Registry{now: now, entries: map[string]*activityRecord{}}
}

// Handle reports the progress and outcome of one backend operation
// started through Registry.Start. Its methods are nil-safe, so code
// built against a service with no activity record does nothing. A handle
// whose entry a fresh start under the same key has superseded also does
// nothing: only the latest start of a key may report for it.
type Handle struct {
	r     *Registry
	key   string
	entry *activityRecord
}

func (r *Registry) Start(key string, kind api.ActivityKind, project config.ProjectID, stream config.WorkstreamID, phase string) *Handle {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	now := r.now()
	e := &activityRecord{ActivityEntry: api.ActivityEntry{ID: key, Kind: kind, Project: project, Workstream: stream, Phase: phase, StartedAt: now, LastProgress: now}}
	r.entries[key] = e
	r.mu.Unlock()
	r.changed()
	return &Handle{r: r, key: key, entry: e}
}

// live reports whether h's entry is still the one the registry holds under
// its key, and runs update on it, under the registry's lock, when it is.
func (h *Handle) live(update func(*activityRecord)) bool {
	h.r.mu.Lock()
	ok := h.r.entries[h.key] == h.entry
	if ok {
		update(h.entry)
	}
	h.r.mu.Unlock()
	return ok
}

// Progress updates the operation's current phase and, when measurable, its
// progress from 0 to 1, and bumps its last-progress time. It has no effect
// once the operation has finished or a fresh start has superseded it.
func (h *Handle) Progress(phase string, progress *float64) {
	if h == nil {
		return
	}
	now := h.r.now()
	if h.live(func(e *activityRecord) { e.Phase, e.Progress, e.LastProgress, e.reported = phase, progress, now, true }) {
		h.r.changed()
	}
}

// Finish ends the operation h reports. A nil error removes the entry at
// once; any other error keeps it, recorded as failed with err's message,
// until a fresh start under the same key supersedes it. It has no effect
// once a fresh start has already superseded h.
func (h *Handle) Finish(err error) {
	if h == nil {
		return
	}
	now := h.r.now()
	if h.live(func(e *activityRecord) {
		if err != nil {
			e.Failure, e.LastProgress = err.Error(), now
		}
	}) {
		if err == nil {
			h.r.mu.Lock()
			delete(h.r.entries, h.key)
			h.r.mu.Unlock()
		}
		h.r.changed()
	}
}

func (r *Registry) changed() {
	if r != nil && r.Notify != nil {
		r.Notify()
	}
}

// snapshot returns every entry, oldest first, as copies readers may hold
// without the registry's lock. A failed entry older than ActivityFailureExpiry
// is dropped from the record as it is read, so a failure nothing retries
// stops being reported, by this call or any later one, once it expires.
func (r *Registry) snapshot() []activityRecord {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make([]activityRecord, 0, len(r.entries))
	for key, e := range r.entries {
		if e.Failure != "" && now.Sub(e.LastProgress) >= ActivityFailureExpiry {
			delete(r.entries, key)
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Status reports the activity record's effect on health: ready unless
// an operation is recovering, failing or stalled, in which case it names the
// operation as the cause. A recovering operation reports not ready; a
// failing or stalled one reports degraded. It never touches the trace mutex
// or operationMu.
//
// "No progress for ActivityStallThreshold" only counts against an entry
// whose kind is stall-tracked (stallTracked) and that has reported progress
// at least once (activityRecord.reported): an entry's own reporting has
// demonstrably started moving, so a later gap of ActivityStallThreshold with
// no further report means it has stopped, not merely that it has not begun.
// Ordinary Git and workspace operations are reconciled as one Apply call
// with no progress report in between (activityOf), so gating on reported
// keeps a Git operation that simply takes longer than the threshold out of
// this rule, while archive cleanup and recovery, which do report progress
// through their phases, are still caught once they stall for real.
//
// An agent turn or a check run is excluded by stallTracked regardless of
// reported: they are reported once at start and once at finish, with
// nothing in between, and routinely run for minutes under this project's
// own checks_timeout and profile timeout — so the same rule would call a
// perfectly healthy turn or check "degraded" for simply still running.
// Those kinds are bounded by their own configured timeout instead: when one
// is exceeded, the wrapped call returns an error and the operation is
// reported failed, which the failure check above already catches.
func (r *Registry) Status() (status, cause string) {
	if r == nil {
		return api.BackendReady, ""
	}
	now := r.now()
	entries := r.snapshot()
	for _, e := range entries {
		if e.Kind == api.ActivityRecovery {
			return api.BackendNotReady, activityCause(e)
		}
	}
	for _, e := range entries {
		if e.Failure != "" {
			return api.BackendDegraded, activityCause(e) + ": " + e.Failure
		}
	}
	for _, e := range entries {
		if stallTracked(e.Kind) && e.reported && now.Sub(e.LastProgress) >= ActivityStallThreshold {
			return api.BackendDegraded, activityCause(e) + " has made no progress since " + e.LastProgress.UTC().Format(time.RFC3339)
		}
	}
	return api.BackendReady, ""
}

// stallTracked reports whether kind is subject to the no-progress stall
// rule in Status. Agent turns and checks are excluded: they report
// only start and finish, run for minutes by design, and are bounded by
// their own configured timeout rather than by elapsed time since the last
// activity report.
func stallTracked(kind api.ActivityKind) bool {
	return kind != api.ActivityAgentTurn && kind != api.ActivityCheck
}

func activityCause(e activityRecord) string {
	if e.Workstream != "" {
		return string(e.Kind) + " (" + string(e.Workstream) + "): " + e.Phase
	}
	if e.Project != "" {
		return string(e.Kind) + " (" + string(e.Project) + "): " + e.Phase
	}
	return string(e.Kind) + ": " + e.Phase
}

// View returns the current operations as independent snapshot entries.
func (r *Registry) View() []api.ActivityEntry {
	entries := r.snapshot()
	var activity []api.ActivityEntry
	if entries != nil {
		activity = make([]api.ActivityEntry, len(entries))
		for i, entry := range entries {
			activity[i] = entry.ActivityEntry
		}
	}
	return activity
}

// ForgetSession removes activity entries for a destroyed session.
func (r *Registry) ForgetSession(project config.ProjectID, stream config.WorkstreamID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	for key, entry := range r.entries {
		if entry.Project == project && entry.Workstream == stream {
			delete(r.entries, key)
		}
	}
	r.mu.Unlock()
	r.changed()
}
