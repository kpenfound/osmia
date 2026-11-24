package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kpenfound/osmia/internal/activity"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/reconcile"
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
func (s *Service) StartActivity(key string, kind ActivityKind, project config.ProjectID, stream config.WorkstreamID, phase string) *activity.Handle {
	return s.activity.Start(key, kind, project, stream, phase)
}

func (s *Service) activityView() ActivityResponse {
	return ActivityResponse{Activity: s.activity.View()}
}

func (s *Service) healthView(ctx context.Context) HealthResponse {
	backend, cause := s.backendStatus()
	return HealthResponse{Ready: backend == api.BackendReady, API: true, Backend: backend, Cause: cause,
		Service: "osmia", APIVersion: 1, Build: s.options.Build, Tailnet: s.tailnetStatus(ctx)}
}

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

var _ reconcile.Bounded = activityReconciler{}

func (a activityReconciler) Bound(op coreadapter.Operation) bool {
	b, ok := a.Reconciler.(reconcile.Bounded)
	return ok && b.Bound(op)
}

func (a activityReconciler) Failing(ctx context.Context, op coreadapter.Operation, failures int, reason string) error {
	if b, ok := a.Reconciler.(reconcile.Bounded); ok {
		return b.Failing(ctx, op, failures, reason)
	}
	return nil
}

func (a activityReconciler) GiveUp(ctx context.Context, op coreadapter.Operation, why reconcile.GiveUpReason) (coreadapter.OperationResult, error) {
	if b, ok := a.Reconciler.(reconcile.Bounded); ok {
		return b.GiveUp(ctx, op, why)
	}
	return coreadapter.OperationResult{}, fmt.Errorf("operation %q is not bounded", op.Action)
}

func activityOf(op coreadapter.Operation) (ActivityKind, config.WorkstreamID, string) {
	switch op.Action {
	case thread.TurnAction:
		var in thread.TurnInput
		if json.Unmarshal(op.Input, &in) == nil {
			return ActivityAgentTurn, in.Workstream, in.Agent + " turn " + in.Turn
		}
		return ActivityAgentTurn, "", "agent turn"
	case CheckAction:
		var in execution.CheckInput
		if json.Unmarshal(op.Input, &in) == nil {
			return ActivityCheck, in.Workstream, fmt.Sprintf("checking unit %s (run %d)", in.Unit, in.Run)
		}
		return ActivityCheck, "", "running checks"
	}
	if op.Boundary == coreadapter.RunnerBoundary {
		return ActivityAgentTurn, "", op.Action
	}
	return api.ActivityGit, "", op.Action
}

func (s *Service) backendStatus() (string, string) { return s.activity.Status() }
