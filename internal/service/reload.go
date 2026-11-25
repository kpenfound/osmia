package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

type scheduleHook struct {
	name string
	pass func(context.Context) error
}

type stages struct {
	cfg        *config.Config
	hooks      []scheduleHook
	polls      []scheduleHook
	runner     runnerAdapter
	repository repositoryAdapter
}

type pipeline struct {
	current, next atomic.Pointer[stages]
	slowThreshold time.Duration
	before        func(string) error
	conversation  atomic.Pointer[stages]
}

func (p *pipeline) schedule(ctx context.Context) error {
	ctx = router.OnSchedulingPath(ctx)
	if next := p.next.Swap(nil); next != nil {
		p.current.Store(next)
	}
	current := p.current.Load()
	for _, hook := range current.hooks {
		started := time.Now()
		var err error
		if p.before != nil {
			err = p.before("schedule:" + hook.name)
		}
		if err == nil {
			err = hook.pass(ctx)
		}
		threshold := p.slowThreshold
		if threshold <= 0 {
			threshold = trace.SlowThreshold
		}
		if duration := time.Since(started); duration >= threshold {
			ghlog.Logger(ctx).Warn("scheduling step exceeded threshold", "project", string(current.cfg.Project.ID), "step", hook.name, "duration", duration.String())
		}
		if err != nil {
			ghlog.Logger(ctx).Error("reconciliation pass hook failed", "project", string(current.cfg.Project.ID), "hook", hook.name, "error", err.Error())
			return fmt.Errorf("%s pass: %w", hook.name, err)
		}
	}
	return nil
}

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

var _ reconcile.Bounded = stagedAdapter{}

func (a stagedAdapter) Bound(op coreadapter.Operation) bool {
	b, ok := a.pick(a.pipeline.current.Load()).(reconcile.Bounded)
	return ok && b.Bound(op)
}

func (a stagedAdapter) Failing(ctx context.Context, op coreadapter.Operation, failures int, reason string) error {
	if b, ok := a.pick(a.pipeline.current.Load()).(reconcile.Bounded); ok {
		return b.Failing(ctx, op, failures, reason)
	}
	return nil
}

func (a stagedAdapter) GiveUp(ctx context.Context, op coreadapter.Operation, why reconcile.GiveUpReason) (coreadapter.OperationResult, error) {
	if b, ok := a.pick(a.pipeline.current.Load()).(reconcile.Bounded); ok {
		return b.GiveUp(ctx, op, why)
	}
	return coreadapter.OperationResult{}, fmt.Errorf("operation %q is not bounded", op.Action)
}

func (s *Service) reload() (ReloadResponse, *APIError) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	cfg, projects := s.runtimes()
	next, restart, err := s.candidate(cfg)
	if err != nil {
		failed := reloadError(err, s.now())
		s.mu.Lock()
		s.reloadErr = failed
		s.mu.Unlock()
		s.hub.Publish(Event{Kind: EventConfig})
		s.log().Error("reload failed", "error", failed.Message)
		return ReloadResponse{}, &APIError{Code: Validation, Message: failed.Message + "; the loaded configuration is unchanged"}
	}
	unchanged := &APIError{Code: Internal, Message: "cannot apply the reloaded configuration to the running project; the loaded configuration is unchanged"}
	failUnchanged := func() (ReloadResponse, *APIError) {
		s.log().Error("reload failed", "error", unchanged.Message)
		return ReloadResponse{}, unchanged
	}
	staged := map[*activeProject]*stages{}
	for _, p := range projects {
		if !next.Active(p.id) {
			continue
		}
		if staged[p], err = s.stages(next.For(p.id), p.repository); err != nil {
			return failUnchanged()
		}
	}
	if err := s.ensureEngine(context.Background(), next); err != nil {
		unchanged.Message = redact.Secrets(err.Error(), redact.KnownSecrets()...) + "; the loaded configuration is unchanged"
		return failUnchanged()
	}
	s.mu.Lock()
	if err := s.resolveRuntime(next, projects); err != nil {
		s.mu.Unlock()
		return failUnchanged()
	}
	s.cfg, s.reloadErr = next, nil
	s.capacity.Store(&next.Capacity)
	reloadedAt := s.now()
	s.checkSlots.resize(next.Capacity.Checks)
	s.chief.resize(next.Capacity.Chief)
	for p, stage := range staged {
		p.pipeline.next.Store(stage)
		p.pipeline.conversation.Store(stage)
	}
	s.mu.Unlock()
	s.spans.reload(reloadedAt, poolsOf(next.Capacity))
	events := []Event{{Kind: EventResync}, {Kind: EventConfig}, {Kind: EventRuntime}}
	for _, id := range next.ProjectIDs() {
		events = append(events, Event{Kind: EventSpend, Project: id})
	}
	if len(next.Projects) == 0 {
		events = append(events, Event{Kind: EventSpend})
	}
	s.reporter.configure(next)
	s.hub.Publish(events...)
	s.log().Info("reload succeeded", "digest", digest(next), "restart_required", len(restart) > 0)
	return ReloadResponse{Digest: digest(next), RestartRequired: restart}, nil
}

func (s *Service) candidate(cfg *config.Config) (*config.Config, []string, error) {
	top, err := config.LoadTopLevel(s.options.Config)
	if err != nil {
		return nil, nil, err
	}
	registerHearsaySecrets(top.Hearsay)
	next := top.WithLoadedProjects(s.effectiveProjects(top, cfg))
	restart := []string{}
	if next.Listen.Socket != cfg.Listen.Socket {
		restart = append(restart, "listen.socket")
	}
	if next.Listen.Web != cfg.Listen.Web {
		restart = append(restart, "listen.web")
	}
	if next.Listen.Tailnet != cfg.Listen.Tailnet {
		restart = append(restart, "listen.tailnet")
	}
	if next.Listen.TailnetHTTPS != cfg.Listen.TailnetHTTPS {
		restart = append(restart, "listen.tailnet_https")
	}
	if next.Listen != cfg.Listen {
		pinned := *next
		pinned.Listen = cfg.Listen
		next = &pinned
	}
	if next.Telemetry != cfg.Telemetry {
		restart = append(restart, "telemetry")
		pinned := *next
		pinned.Telemetry = cfg.Telemetry
		next = &pinned
	}
	return next, restart, nil
}

func (s *Service) effectiveProjects(top, cfg *config.Config) []config.Project {
	revisions := s.effectiveRevisionsNow()
	store := projectstore.Open(cfg.Root)
	out := make([]config.Project, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		if number, ok := revisions[p.ID]; ok {
			if revision, err := store.At(p.ID, number); err == nil {
				if file, err := top.DecodeProject(p.ID, s.options.Config.Home, revision.Data); err == nil {
					p = file.Project
				}
			}
		}
		out = append(out, p)
	}
	return out
}

func reloadError(err error, at time.Time) *ReloadError {
	var field *config.FieldError
	if errors.As(err, &field) {
		return &ReloadError{Path: field.Path, Field: field.Field, Message: field.Error(), At: at}
	}
	return &ReloadError{Message: "the configuration file cannot be located or read; check the top-level config.toml and its permissions", At: at}
}
