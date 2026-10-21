package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
)

// StartActivity records the start of a long-running backend operation in the
// service's activity record and returns a handle to report its phase,
// progress and outcome. key identifies "the same operation" across retries,
// such as a reconciled operation's stable ID or a fixed name for a startup
// recovery step; starting a key again supersedes whatever it last reported,
// finished or not. kind and phase are free text naming the operation and its
// current step; project and stream name the trace and workstream it
// concerns, when known.
func (s *Service) StartActivity(key string, kind ActivityKind, project config.ProjectID, stream config.WorkstreamID, phase string) *ActivityHandle {
	return s.activity.Start(key, kind, project, stream, phase)
}

// activityView answers GET /v1/activity: a snapshot of the activity record,
// read without touching the trace mutex or operationMu.
func (s *Service) activityView() ActivityResponse {
	return ActivityResponse{Activity: s.activity.View()}
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

// activityReconciler reports every call to Apply as one backend operation in
// the activity record, keyed by the operation's own stable ID so a retried
// attempt supersedes what an earlier one reported: started before the
// wrapped reconciler runs, named after the operation's action and, for a
// queued thread turn or a unit's checks, its workstream, and finished,
// successfully or not, once Apply returns. project is the one trace this
// adapter reconciles.
type activityReconciler struct {
	coreadapter.Reconciler
	activity *activity.Registry
	project  config.ProjectID
}

func (a activityReconciler) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	kind, stream, phase := activityOf(op)
	h := a.activity.Start(op.ID, kind, a.project, stream, phase)
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

// backendStatus reports health from the activity registry alone.
func (s *Service) backendStatus() (string, string) { return s.activity.Status() }

type ActivityHandle = activity.Handle

const ActivityStallThreshold = activity.ActivityStallThreshold
const ActivityFailureExpiry = activity.ActivityFailureExpiry
