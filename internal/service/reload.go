package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// scheduleHook is one step of the schedule a pass runs before it reconciles
// operations.
type scheduleHook struct {
	name string
	pass func(context.Context) error
}

// stages is the reconciliation built from one loaded configuration: the
// schedule hooks every pass runs and the adapters that reconcile operations.
type stages struct {
	cfg        *config.Config
	hooks      []scheduleHook
	runner     runnerAdapter
	repository repositoryAdapter
}

// pipeline holds the stages of a project's passes. A reload stores the
// reloaded configuration's stages in next, and the next pass adopts them
// before its first hook, so an operation already running finishes on the
// stages it started with.
type pipeline struct {
	current, next atomic.Pointer[stages]
}

func (p *pipeline) schedule(ctx context.Context) error {
	if next := p.next.Swap(nil); next != nil {
		p.current.Store(next)
	}
	for _, hook := range p.current.Load().hooks {
		if err := hook.pass(ctx); err != nil {
			return fmt.Errorf("%s pass: %w", hook.name, err)
		}
	}
	return nil
}

// stagedAdapter reconciles an operation through one adapter of the
// pipeline's current stages.
type stagedAdapter struct {
	pipeline *pipeline
	pick     func(*stages) coreadapter.Reconciler
}

func (a stagedAdapter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	return a.pick(a.pipeline.current.Load()).Inspect(ctx, op)
}
func (a stagedAdapter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	return a.pick(a.pipeline.current.Load()).Apply(ctx, op)
}

// reload reads the top-level configuration and every registered project's
// file as one candidate and replaces the loaded configuration with it only
// when all of them pass. A failure is kept as the last reload error until a
// reload succeeds. Settings that need a restart keep their loaded values and
// are named in the response. The runtime state is resolved against the
// candidate, never rewritten, and the running project's passes adopt the
// candidate's stages from their next pass.
func (s *Service) reload() (ReloadResponse, *APIError) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	s.mu.Lock()
	cfg, active := s.cfg, s.active
	s.mu.Unlock()
	next, restart, err := s.candidate(cfg)
	if err != nil {
		failed := reloadError(err, s.now())
		s.mu.Lock()
		s.reloadErr = failed
		s.mu.Unlock()
		return ReloadResponse{}, &APIError{Validation, failed.Message + "; the loaded configuration is unchanged"}
	}
	unchanged := &APIError{Internal, "cannot apply the reloaded configuration to the running project; the loaded configuration is unchanged"}
	var (
		staged     *stages
		repository *trace.Repository
	)
	if active != nil {
		repository = active.repository
		if staged, err = s.stages(next, repository); err != nil {
			return ReloadResponse{}, unchanged
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.resolveRuntime(next, repository); err != nil {
		return ReloadResponse{}, unchanged
	}
	s.cfg, s.reloadErr = next, nil
	if staged != nil {
		active.pipeline.next.Store(staged)
	}
	return ReloadResponse{Digest: digest(next), RestartRequired: restart}, nil
}

// candidate loads the configuration on disk as a reload applies it over cfg.
// Changed settings that need a restart keep cfg's values and are named: the
// listen socket, the web listener, and the active project list, which only
// project add and remove change in a running service. The loaded project's
// file is validated even when the disk lists another project.
func (s *Service) candidate(cfg *config.Config) (*config.Config, []string, error) {
	next, err := config.Load(s.options.Config)
	if err != nil {
		return nil, nil, err
	}
	restart := []string{}
	if next.Project.ID != cfg.Project.ID {
		restart = append(restart, "active_projects")
		if cfg.HasProject() {
			if next, err = next.WithProject(cfg.Project.ID, s.options.Config.Home); err != nil {
				return nil, nil, err
			}
		} else {
			next = next.WithoutProject()
		}
	}
	if next.Listen.Socket != cfg.Listen.Socket {
		restart = append(restart, "listen.socket")
	}
	if next.Listen.Web != cfg.Listen.Web {
		restart = append(restart, "listen.web")
	}
	if next.Listen != cfg.Listen {
		pinned := *next
		pinned.Listen = cfg.Listen
		next = &pinned
	}
	return next, restart, nil
}

// reloadError describes a configuration that failed to load by its file and
// field, never by values read from it.
func reloadError(err error, at time.Time) *ReloadError {
	var field *config.FieldError
	if errors.As(err, &field) {
		return &ReloadError{Path: field.Path, Field: field.Field, Message: field.Error(), At: at}
	}
	return &ReloadError{Message: "the configuration files cannot be located or read; check config.toml and projects under the root and their permissions", At: at}
}
