package trace

import (
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// turnScope requires r.mu. It returns the thread and turn a tool scope names;
// active also requires this session's uncaptured claim on that turn.
func (r *Repository) turnScope(agent string, scope coreadapter.Scope, active bool) (Thread, QueuedTurn, error) {
	if scope.Project != string(r.project) || !key(agent) || !key(scope.Role) || !key(scope.Thread) || !key(scope.Turn) {
		return Thread{}, QueuedTurn{}, fmt.Errorf("turn scope denied")
	}
	stream := config.WorkstreamID(scope.Workstream)
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return Thread{}, QueuedTurn{}, err
	}
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return Thread{}, QueuedTurn{}, err
	}
	t, ok := log.Threads[agent]
	if !ok || t.Identity.Role != scope.Role || t.Identity.ThreadID != scope.Thread || (active && t.Active != scope.Turn) {
		return Thread{}, QueuedTurn{}, fmt.Errorf("turn scope denied")
	}
	for _, q := range t.Turns {
		if q.Request.TurnID == scope.Turn && q.Request.Unit == scope.Unit {
			if active && (q.Claim == nil || q.Claim.ServiceSession != r.session || q.Response != nil) {
				return Thread{}, QueuedTurn{}, ErrClaim
			}
			return t, q, nil
		}
	}
	return Thread{}, QueuedTurn{}, fmt.Errorf("turn not found")
}

// ActiveTurn requires the exact role, thread, unit and current service claim.
// It lets service-owned auxiliary tools share the workflow's execution boundary.
func (r *Repository) ActiveTurn(scope coreadapter.Scope) error {
	r, err := r.scoped(config.WorkstreamID(scope.Workstream))
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if scope.Project != string(r.project) {
		return fmt.Errorf("turn scope denied")
	}
	log, _, err := r.loadWorkflow(config.WorkstreamID(scope.Workstream))
	if err != nil {
		return err
	}
	for agent, t := range log.Threads {
		if t.Identity.ThreadID == scope.Thread && t.Identity.Role == scope.Role {
			_, _, err := r.turnScope(agent, scope, true)
			return err
		}
	}
	return fmt.Errorf("turn scope denied")
}
