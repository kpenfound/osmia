package service

import (
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	kindregistry "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/trace"
)

// ClimbAttemptsResponse is a Hill Climb workstream's attempt log: every
// recorded attempt, in attempt order, as it is stored at
// climb/attempts/<n>.json.
type ClimbAttemptsResponse struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Attempts   []climb.Attempt     `json:"attempts"`
}

// ClimbProgressResponse is the kind's progress read for a Hill Climb
// workstream: what the climb panel draws.
type ClimbProgressResponse struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Progress   climb.Progress      `json:"progress"`
}

func (s *Service) climbTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	project, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return "", "", nil, api
	}
	kind, err := repository.Kind(stream)
	if err != nil {
		return "", "", nil, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the kind of workstream %s; check the trace repository", stream)}
	}
	if kind != kindregistry.HillClimb {
		return "", "", nil, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is a %s workstream and has no climb", stream, kind)}
	}
	return project, stream, repository, nil
}

func (s *Service) climbAttempts(raw string) (ClimbAttemptsResponse, *APIError) {
	_, stream, repository, api := s.climbTrace(raw)
	if api != nil {
		return ClimbAttemptsResponse{}, api
	}
	attempts, err := climb.ReadAttempts(repository, stream)
	if err != nil {
		return ClimbAttemptsResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the attempts of workstream %s; check the trace repository", stream)}
	}
	if attempts == nil {
		attempts = []climb.Attempt{}
	}
	return ClimbAttemptsResponse{Workstream: stream, Attempts: attempts}, nil
}

func (s *Service) climbProgress(raw string) (ClimbProgressResponse, *APIError) {
	project, stream, repository, api := s.climbTrace(raw)
	if api != nil {
		return ClimbProgressResponse{}, api
	}
	cfg, _ := s.runtimeOf(project)
	progress, err := readClimbProgress(repository, cfg, stream, true, time.Now())
	if err != nil {
		return ClimbProgressResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the climb of workstream %s cannot be read yet: %v", stream, err)}
	}
	return ClimbProgressResponse{Workstream: stream, Progress: progress}, nil
}

// readClimbProgress reads a Hill Climb workstream's progress from its sealed
// climb spec and the records the climb has written. The cost meter reads the
// workstream's cost records only when withCost is set.
func readClimbProgress(repository *trace.Repository, cfg *config.Config, stream config.WorkstreamID, withCost bool, now time.Time) (climb.Progress, error) {
	spec, _, err := intake.SealedClimbSpec(repository, cfg, stream)
	if err != nil {
		return climb.Progress{}, err
	}
	in := climb.ProgressInput{Spec: spec, Now: now}
	if in.Attempts, err = climb.ReadAttempts(repository, stream); err != nil {
		return climb.Progress{}, err
	}
	inner, err := climb.ReadInner(repository, stream)
	if err != nil {
		return climb.Progress{}, err
	}
	approval, approved, err := climb.ReadApproval(repository, stream)
	if err != nil {
		return climb.Progress{}, err
	}
	in.Epoch = 1
	retired := false
	if approved {
		in.Epoch = approval.EpochNumber()
		if inner.Value != climb.InnerClimbing && inner.Value != climb.InnerConcluding {
			in.Epoch++
			retired = inner.Value == climb.InnerCalibrating
		}
	}
	budget, err := climb.ReadBudget(repository, stream)
	if err != nil {
		return climb.Progress{}, err
	}
	in.Extension = budget.Total()
	if calibration, _, found, err := climb.ReadCalibration(repository, stream); err != nil {
		return climb.Progress{}, err
	} else if found && !retired {
		in.Calibration, in.SpecRevision = &calibration.Calibration, calibration.SpecRevision
	}
	if running, found, err := climb.ReadRunning(repository, stream); err != nil {
		return climb.Progress{}, err
	} else if found {
		in.Running = &running
	}
	if withCost {
		costs, err := trace.Read[trace.Cost](repository, stream)
		if err != nil {
			return climb.Progress{}, err
		}
		for _, c := range costs {
			if c.Entry.Usage.CostKnown {
				in.CostUSD += c.Entry.Usage.CostUSD
			}
		}
	}
	return climb.BuildProgress(in), nil
}

// climbProgressView is the progress a Hill Climb workstream shows in the
// workstream list and in osmia status. A climb that cannot be read yet, such
// as one that is not sealed, shows the panel with no summary.
func climbProgressView(repository *trace.Repository, cfg *config.Config, stream config.WorkstreamID, now time.Time) *api.ProgressView {
	view := &api.ProgressView{Panel: "climb"}
	if progress, err := readClimbProgress(repository, cfg, stream, false, now); err == nil {
		view.Summary, view.Spark = progress.Summary, progress.Spark
	}
	return view
}
