package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) abandon(ctx context.Context, raw string, req AbandonRequest) (AbandonResponse, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return AbandonResponse{}, api
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return AbandonResponse{}, &APIError{Code: Validation, Message: "reason must not be empty"}
	}
	if joining, err := s.intake().SelectionJoiningMember(stream); err != nil {
		return AbandonResponse{}, &APIError{Code: Internal, Message: err.Error()}
	} else if joining {
		return AbandonResponse{}, &APIError{Code: Conflict, Message: "the workgroup is still joining its owner-selected members; cancel the whole workgroup after it has joined"}
	}
	if api := s.lifecycle().MemberRemovalGate(ctx, raw); api != nil {
		return AbandonResponse{}, api
	}
	failed := &APIError{Code: Internal, Message: fmt.Sprintf("cannot abandon workstream %s; check the trace repository", stream)}
	state, err := repository.RawWorkflow(stream, trace.FeatureSubject)
	if err != nil {
		return AbandonResponse{}, failed
	}
	if state.Value == AbandonedState || state.Value == DeliveredState {
		return AbandonResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is %s and cannot be abandoned", stream, state.Value)}
	}
	body := fmt.Sprintf("Workstream state changed from %s to %s: %s", state.Value, AbandonedState, reason)
	if state.Value == "" {
		body = fmt.Sprintf("Workstream state changed to %s: %s", AbandonedState, reason)
	}
	res, err := s.RouteExit(ctx, intake.PinnedMove{
		Repo: repository, Stream: stream, Subject: trace.FeatureSubject, State: state,
		Trigger: intake.TriggerAbandon, Cause: abandonTransition, Target: AbandonedState, Description: "Abandon the workstream.", Gate: true,
		Ref: abandonTransition, ID: abandonTransition, HeaderCause: abandonTransition, Reason: reason, Actor: ownerActor, At: s.now(),
		Events: []trace.Event{trace.Progress(abandonTransition, "state", body)},
	}.Exit())
	switch {
	case err != nil:
		s.log().Warn("cannot record the abandonment of a workstream", "workstream", string(stream), "error", err.Error())
		return AbandonResponse{}, failed
	case res.Hold == router.HoldStale:
		return AbandonResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s changed state while abandoning it; check osmia status and retry", stream)}
	case !res.Committed:
		return AbandonResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s was not abandoned: the router held it (%s). %s", stream, res.Hold, res.Reason)}
	}
	s.closeFlakeFix(ctx, repository, stream)
	s.turns.cancel(stream)
	if _, err := repository.CancelTurns(context.WithoutCancel(ctx), stream, s.now(), abandonActor, cancelReason); err != nil {
		return AbandonResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("workstream %s is abandoned but its queued turns could not be cancelled; they are cancelled at the next start. Check the trace repository", stream)}
	}
	return AbandonResponse{Project: project, Workstream: stream, State: AbandonedState, Reason: reason}, nil
}

func cancelAbandoned(ctx context.Context, repository *trace.Repository, at func() time.Time) error {
	streams, err := repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		gone, err := abandoned(repository, stream)
		if err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		}
		if !gone {
			continue
		}
		if _, err := repository.CancelTurns(ctx, stream, at(), abandonActor, cancelReason); err != nil {
			return fmt.Errorf("workstream %s: %w", stream, err)
		}
	}
	return nil
}

type runningTurns struct {
	mu      sync.Mutex
	next    int
	streams map[config.WorkstreamID]map[int]runningTurn
}

type runningTurn struct {
	project config.ProjectID
	cancel  context.CancelFunc
	stop    func(*thread.Stop)
}

func (r *runningTurns) add(stream config.WorkstreamID, cancel context.CancelFunc) func() {
	return r.track(stream, runningTurn{cancel: cancel})
}

func (r *runningTurns) track(stream config.WorkstreamID, turn runningTurn) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streams == nil {
		r.streams = map[config.WorkstreamID]map[int]runningTurn{}
	}
	if r.streams[stream] == nil {
		r.streams[stream] = map[int]runningTurn{}
	}
	r.next++
	id := r.next
	r.streams[stream][id] = turn
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.streams[stream], id)
	}
}

func (r *runningTurns) cancel(stream config.WorkstreamID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, turn := range r.streams[stream] {
		if turn.cancel != nil {
			turn.cancel()
		}
	}
}

func (r *runningTurns) stopStream(stream config.WorkstreamID, stop *thread.Stop) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	stopped := 0
	for _, turn := range r.streams[stream] {
		if turn.stop != nil {
			turn.stop(stop)
			stopped++
		}
	}
	return stopped
}

func (r *runningTurns) stop(pauses []runtime.Pause) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	stopped := 0
	for stream, turns := range r.streams {
		for _, turn := range turns {
			if turn.stop == nil {
				continue
			}
			if p, ok := scheduler.Stopping(pauses, turn.project, stream); ok {
				turn.stop(pauseStop(p))
				stopped++
			}
		}
	}
	return stopped
}

type abandonable struct {
	coreadapter.Reconciler
	s          *Service
	repository *trace.Repository
	cfg        *config.Config
}

func (a abandonable) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := thread.DecodeTurn(op)
	if err != nil {
		return a.Reconciler.Apply(ctx, op)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	running := runningTurn{project: a.repository.Project(), cancel: cancel}
	th, err := a.repository.Thread(in.Workstream, in.Agent)
	if err == nil && !trace.ManagerLike(th.Identity.Role) {
		ctx, running.stop = thread.Stoppable(ctx)
	}
	defer a.s.turns.track(in.Workstream, running)()
	if running.stop != nil && a.s.store != nil {
		state, _ := a.s.effective()
		if p, ok := scheduler.Stopping(state.Pauses, running.project, in.Workstream); ok {
			running.stop(pauseStop(p))
		}
	}
	gone, err := abandoned(a.repository, in.Workstream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if gone {
		if _, err := a.repository.CancelTurns(ctx, in.Workstream, a.s.now(), abandonActor, cancelReason); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	role, profile, unit := describeQueuedTurn(th, in.Turn)
	if a.cfg != nil && (role == masonRole || role == sessionRole) && a.cfg.RoleExecution(masonRole).Dagger != nil {
		release, err := a.s.checkSlots.startEngineTurn(ctx, EngineTurn{Project: running.project, Workstream: in.Workstream, Unit: unit, Agent: in.Agent, Turn: in.Turn})
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		defer release()
	}
	attrs := []any{"project", string(a.repository.Project()), "workstream", string(in.Workstream), "role", role, "profile", profile}
	if unit != "" {
		attrs = append(attrs, "unit", unit)
	}
	logger := ghlog.Logger(ctx)
	logger.Info("agent turn dispatched", attrs...)
	var claimToken string
	ctx = thread.WithClaimObserver(ctx, func(req trace.TurnRequest, claim trace.TurnClaim) {
		if req.AgentID == in.Agent && req.TurnID == in.Turn {
			claimToken = claim.Token
		}
	})
	start := a.s.now()
	result, err := a.Reconciler.Apply(ctx, op)
	finished := append(append([]any{}, attrs...), "duration", a.s.now().Sub(start).String())
	switch {
	case err != nil:
		logger.Error("agent turn finished", append(finished, "outcome", "failed", "error", err.Error())...)
	default:
		outcome := result.Outcome
		if outcome == "" {
			outcome = "ok"
		}
		logger.Info("agent turn finished", append(finished, "outcome", outcome)...)
	}
	if gone, _ := abandoned(a.repository, in.Workstream); gone {
		if claimToken != "" {
			if _, cancelErr := a.repository.CancelReturnedTurn(context.WithoutCancel(ctx), in.Workstream, in.Agent, in.Turn, claimToken, a.s.now(), abandonActor, cancelReason); cancelErr != nil {
				return result, errors.Join(err, cancelErr)
			}
		}
		if _, cancelErr := a.repository.CancelTurns(context.WithoutCancel(ctx), in.Workstream, a.s.now(), abandonActor, cancelReason); cancelErr != nil {
			return result, errors.Join(err, cancelErr)
		}
	}
	return result, err
}

func describeQueuedTurn(th trace.Thread, turnID string) (role, profile, unit string) {
	role = th.Identity.Role
	for _, q := range th.Turns {
		if q.Request.TurnID == turnID {
			return role, q.Request.Profile.Name, q.Request.Unit
		}
	}
	return role, "", ""
}
