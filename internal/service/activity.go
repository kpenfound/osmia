package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
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
	ActivityEntry
	reported bool
}

// activityRegistry is the service's in-process record of long-running
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
type activityRegistry struct {
	now    func() time.Time
	notify func()

	mu      sync.Mutex
	entries map[string]*activityRecord
}

func newActivityRegistry(now func() time.Time) *activityRegistry {
	return &activityRegistry{now: now, entries: map[string]*activityRecord{}}
}

// ActivityHandle reports the progress and outcome of one backend operation
// started through Service.StartActivity. Its methods are nil-safe, so code
// built against a service with no activity record does nothing. A handle
// whose entry a fresh start under the same key has superseded also does
// nothing: only the latest start of a key may report for it.
type ActivityHandle struct {
	r     *activityRegistry
	key   string
	entry *activityRecord
}

// StartActivity records the start of a long-running backend operation in the
// service's activity record and returns a handle to report its phase,
// progress and outcome. key identifies "the same operation" across retries,
// such as a reconciled operation's stable ID or a fixed name for a startup
// recovery step; starting a key again supersedes whatever it last reported,
// finished or not. kind and phase are free text naming the operation and its
// current step; project and stream name the trace and workstream it
// concerns, when known.
func (s *Service) StartActivity(key string, kind ActivityKind, project config.ProjectID, stream config.WorkstreamID, phase string) *ActivityHandle {
	return s.activity.start(key, kind, project, stream, phase)
}

func (r *activityRegistry) start(key string, kind ActivityKind, project config.ProjectID, stream config.WorkstreamID, phase string) *ActivityHandle {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	now := r.now()
	e := &activityRecord{ActivityEntry: ActivityEntry{ID: key, Kind: kind, Project: project, Workstream: stream, Phase: phase, StartedAt: now, LastProgress: now}}
	r.entries[key] = e
	r.mu.Unlock()
	r.changed()
	return &ActivityHandle{r: r, key: key, entry: e}
}

// live reports whether h's entry is still the one the registry holds under
// its key, and runs update on it, under the registry's lock, when it is.
func (h *ActivityHandle) live(update func(*activityRecord)) bool {
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
func (h *ActivityHandle) Progress(phase string, progress *float64) {
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
func (h *ActivityHandle) Finish(err error) {
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

func (r *activityRegistry) changed() {
	if r != nil && r.notify != nil {
		r.notify()
	}
}

// snapshot returns every entry, oldest first, as copies readers may hold
// without the registry's lock. A failed entry older than ActivityFailureExpiry
// is dropped from the record as it is read, so a failure nothing retries
// stops being reported, by this call or any later one, once it expires.
func (r *activityRegistry) snapshot() []activityRecord {
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

// activityView answers GET /v1/activity: a snapshot of the activity record,
// read without touching the trace mutex or operationMu.
func (s *Service) activityView() ActivityResponse {
	entries := s.activity.snapshot()
	var activity []ActivityEntry
	if entries != nil {
		activity = make([]ActivityEntry, len(entries))
		for i, entry := range entries {
			activity[i] = entry.ActivityEntry
		}
	}
	return ActivityResponse{Activity: activity}
}

// healthView answers GET /v1/health: the API is available whenever this
// response is built at all, since it touches neither the trace nor
// anything that waits on it; Backend and Cause come from the activity
// record alone.
func (s *Service) healthView(ctx context.Context) HealthResponse {
	backend, cause := s.backendStatus()
	return HealthResponse{Ready: backend == BackendReady, API: true, Backend: backend, Cause: cause,
		Service: "osmia", APIVersion: 1, Build: s.options.Build, Tailnet: s.tailnetStatus(ctx)}
}

// backendStatus reports the activity record's effect on health: ready unless
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
func (s *Service) backendStatus() (status, cause string) {
	now := s.now()
	entries := s.activity.snapshot()
	for _, e := range entries {
		if e.Kind == ActivityRecovery {
			return BackendNotReady, activityCause(e)
		}
	}
	for _, e := range entries {
		if e.Failure != "" {
			return BackendDegraded, activityCause(e) + ": " + e.Failure
		}
	}
	for _, e := range entries {
		if stallTracked(e.Kind) && e.reported && now.Sub(e.LastProgress) >= ActivityStallThreshold {
			return BackendDegraded, activityCause(e) + " has made no progress since " + e.LastProgress.UTC().Format(time.RFC3339)
		}
	}
	return BackendReady, ""
}

// stallTracked reports whether kind is subject to the no-progress stall
// rule in backendStatus. Agent turns and checks are excluded: they report
// only start and finish, run for minutes by design, and are bounded by
// their own configured timeout rather than by elapsed time since the last
// activity report.
func stallTracked(kind ActivityKind) bool {
	return kind != ActivityAgentTurn && kind != ActivityCheck
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

// activityReconciler reports every call to Apply as one backend operation in
// the activity record, keyed by the operation's own stable ID so a retried
// attempt supersedes what an earlier one reported: started before the
// wrapped reconciler runs, named after the operation's action and, for a
// queued thread turn or a unit's checks, its workstream, and finished,
// successfully or not, once Apply returns. project is the one trace this
// adapter reconciles.
type activityReconciler struct {
	coreadapter.Reconciler
	activity *activityRegistry
	project  config.ProjectID
}

func (a activityReconciler) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	kind, stream, phase := activityOf(op)
	h := a.activity.start(op.ID, kind, a.project, stream, phase)
	result, err := a.Reconciler.Apply(ctx, op)
	h.Finish(err)
	return result, err
}

// activityOf classifies a reconciled operation for the activity record: its
// kind, the workstream it names when the operation's input carries one, and
// a phase describing it.
func activityOf(op coreadapter.Operation) (ActivityKind, config.WorkstreamID, string) {
	switch op.Action {
	case thread.TurnAction:
		var in thread.TurnInput
		if json.Unmarshal(op.Input, &in) == nil {
			return ActivityAgentTurn, in.Workstream, in.Agent + " turn " + in.Turn
		}
		return ActivityAgentTurn, "", "agent turn"
	case CheckAction:
		var in checkInput
		if json.Unmarshal(op.Input, &in) == nil {
			return ActivityCheck, in.Workstream, fmt.Sprintf("checking unit %s (run %d)", in.Unit, in.Run)
		}
		return ActivityCheck, "", "running checks"
	}
	if op.Boundary == coreadapter.RunnerBoundary {
		return ActivityAgentTurn, "", op.Action
	}
	return ActivityGit, "", op.Action
}

// forgetSession removes completed activity failures for a destroyed session.
func (r *activityRegistry) forgetSession(project config.ProjectID, stream config.WorkstreamID) {
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
