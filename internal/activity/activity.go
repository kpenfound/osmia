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

// ActivityFailureExpiry is how long a failed entry degrades health and stays
// listed when nothing starts the operation again under its key.
const ActivityFailureExpiry = 10 * time.Minute

type activityRecord struct {
	api.ActivityEntry
	reported bool
}

// Registry is the service's in-process record of long-running backend
// operations. Entries are keyed by the caller's identity for the same
// operation. A successful finish removes the entry; a failure keeps it until a
// fresh start under the key supersedes it or ActivityFailureExpiry passes. It
// holds no trace or operation lock, so reporting and reading never wait on
// them.
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

// Status reports the record's effect on health. A recovering operation makes
// the backend not ready. A failed operation, or an operation that reported
// progress and then made none for ActivityStallThreshold, makes it degraded.
// Agent turns and checks are exempt from the stall rule; their own timeouts end
// them as failures. Each cause names the operation.
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
