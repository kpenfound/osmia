package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	kindreg "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

// climbRescan is how often the climb worker looks for a Hill Climb workstream
// whose inner state can move, besides the scheduling pass kicking it.
var climbRescan = 2 * time.Second

// climbWorker moves the inner machine of every building Hill Climb workstream
// of a project. Calibration runs the evaluator for minutes, so it is carried
// out here and never on the scheduling path.
type climbWorker struct {
	s      *Service
	active *activeProject
	wake   chan struct{}
	wg     sync.WaitGroup
	mu     sync.Mutex
	busy   map[config.WorkstreamID]bool
}

func newClimbWorker(s *Service, active *activeProject) *climbWorker {
	return &climbWorker{s: s, active: active, wake: make(chan struct{}, 1), busy: map[config.WorkstreamID]bool{}}
}

func (w *climbWorker) kick() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (s *Service) kickClimb(repository *trace.Repository) {
	if w, ok := s.climbWorkers.Load(repository); ok {
		w.(*climbWorker).kick()
	}
}

func (w *climbWorker) run(ctx context.Context) {
	defer w.wg.Wait()
	ticker := time.NewTicker(climbRescan)
	defer ticker.Stop()
	for {
		w.scan(ctx)
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

func (w *climbWorker) scan(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	repository := w.active.repository
	statuses, err := repository.Statuses()
	if err != nil {
		if ctx.Err() == nil {
			w.s.log().Warn("cannot read the workstreams of the climb worker", "project", string(w.active.id), "error", err.Error())
		}
		return
	}
	state, _ := w.s.effective()
	for _, st := range statuses {
		if st.WorkKind != kindreg.HillClimb || st.State != intake.BuildingState {
			continue
		}
		if scheduler.Paused(state.Pauses, w.active.id, st.Workstream) {
			continue
		}
		if held, err := changeHeld(repository, st.Workstream); err != nil || held {
			continue
		}
		stream := st.Workstream
		w.mu.Lock()
		if w.busy[stream] {
			w.mu.Unlock()
			continue
		}
		w.busy[stream] = true
		w.mu.Unlock()
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			defer func() {
				w.mu.Lock()
				delete(w.busy, stream)
				w.mu.Unlock()
			}()
			machine := w.s.climbMachine(repository)
			if err := machine.Reconcile(ctx, stream); err != nil && ctx.Err() == nil {
				w.s.log().Warn("cannot move the climb's inner state", "project", string(w.active.id), "workstream", string(stream), "error", err.Error())
			}
		}()
	}
}

// climbMachine returns the inner machine of the project's Hill Climb
// workstreams, bound to the project's configuration as it is now.
func (s *Service) climbMachine(repository *trace.Repository) *climb.Machine {
	cfg := s.about(repository)
	root := filepath.Join(cfg.Root.String(), "climb", string(repository.Project()))
	return &climb.Machine{
		Repository: repository,
		Spec: func(stream config.WorkstreamID) (climb.Spec, int, error) {
			spec, sealed, err := intake.SealedClimbSpec(repository, cfg, stream)
			return spec, sealed.Revision.Spec, err
		},
		Settings: cfg.HillClimb,
		Workspace: func(stream config.WorkstreamID) (climb.Workspace, error) {
			return intake.FeatureWorkspaces(cfg, repository).Of(stream)
		},
		Branch: intake.FeatureBranch,
		Runner: func(stream config.WorkstreamID, source climb.Source) (climb.Runner, error) {
			engine, ok := s.configuredChecks(cfg).(checks.FunctionCaller)
			if !ok {
				return climb.Runner{}, errors.New("the configured check client cannot call a function of the dev module")
			}
			project := repository.Project()
			return climb.Runner{Source: source, Engine: engine, Root: filepath.Join(root, "measure"), Timeout: cfg.HillClimb().MeasurementTimeout,
				AcquireSlot: func(ctx context.Context) (func(), error) {
					return s.checkSlots.acquire(ctx, CheckSlotRun{Project: project, Workstream: stream, Unit: climb.EvaluatorUnitID})
				}}, nil
		},
		Root: filepath.Join(root, "export"),
		Now:  s.now,
		Step: s.step,
	}
}

// approveClimb records the owner's approval of a Hill Climb workstream's
// calibrated evaluator. It is the one entry to climbing: it is reached only
// through the owner's own API call, never through a gate, so no result of Jev
// can approve the card.
func (s *Service) approveClimb(ctx context.Context, raw string, req api.ClimbApprovalRequest) (api.ClimbApprovalResponse, *APIError) {
	project, stream, repository, traceErr := s.conversationTrace(raw)
	if traceErr != nil {
		return api.ClimbApprovalResponse{}, traceErr
	}
	if req.Decision != "approve" {
		return api.ClimbApprovalResponse{}, &APIError{Code: Validation, Message: fmt.Sprintf("the evaluator approval card offers one decision, approve, not %q", req.Decision)}
	}
	if kind, err := repository.Kind(stream); err != nil {
		return api.ClimbApprovalResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the kind of workstream %s; check the trace repository", stream)}
	} else if kind != kindreg.HillClimb {
		return api.ClimbApprovalResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is not a %s workstream, so it has no evaluator to approve", stream, kindreg.HillClimb)}
	}
	rec, err := s.climbMachine(repository).Approve(ctx, stream, req.Commit, intake.OwnerActor)
	switch {
	case errors.Is(err, climb.ErrNotAwaiting), errors.Is(err, climb.ErrStaleApproval):
		return api.ClimbApprovalResponse{}, &APIError{Code: Conflict, Message: err.Error() + "; read the inbox again"}
	case err != nil:
		return api.ClimbApprovalResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the approval of workstream %s: %v", stream, err)}
	}
	s.kickClimb(repository)
	return api.ClimbApprovalResponse{Project: project, Workstream: stream, Evaluator: rec.Evaluator, Commit: rec.Commit, Baseline: rec.Baseline, Noise: rec.Noise, Protected: slices.Clone(rec.Protected), At: rec.At}, nil
}
