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

// scheduleHook is a named controller step. Stages separate scheduling hooks
// from the observers that run beside the reconciliation pass.
type scheduleHook struct {
	name string
	pass func(context.Context) error
}

// stages holds one loaded configuration's scheduling hooks, background
// observers and operation adapters.
type stages struct {
	cfg        *config.Config
	hooks      []scheduleHook
	polls      []scheduleHook
	runner     runnerAdapter
	repository repositoryAdapter
}

// pipeline holds the stages of a project's passes. A reload stores the
// reloaded configuration's stages in next, and the next pass adopts them
// before its first hook, so an operation already running finishes on the
// stages it started with.
type pipeline struct {
	current, next atomic.Pointer[stages]
	slowThreshold time.Duration
	before        func(string) error
	// Conversations adopt reloads without waiting for a scheduling pass.
	conversation atomic.Pointer[stages]
}

func (p *pipeline) schedule(ctx context.Context) error {
	// Jev is never asked on the scheduling path: the hooks hand their exits to
	// the router's worker.
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

var _ reconcile.Bounded = stagedAdapter{}

// Bound, Failing and GiveUp pass the operation's bounds on from the staged
// adapter when it has them.
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

// reload reads the top-level configuration file and replaces the loaded
// configuration with it, together with each active project's effective
// settings, only when the file passes. A failure is kept as the last reload
// error until a reload succeeds. Settings that need a restart keep their loaded
// values and are named in the response. A reload reads no project settings
// from the store, so a revision that was saved but not adopted stays pending,
// and it never adds or removes a project. The runtime state is resolved
// against the candidate, never rewritten, and every running project's passes
// adopt the candidate's stages from their next pass.
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
	for p, stage := range staged {
		p.pipeline.next.Store(stage)
		p.pipeline.conversation.Store(stage)
	}
	s.mu.Unlock()
	s.spans.reload(reloadedAt, poolsOf(next.Capacity))
	// The runtime's effective profiles and the daily budget's limit follow the
	// configuration.
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

// candidate builds the configuration a reload applies over cfg: the top-level
// file as it is on disk, with cfg's active projects, in registry order, each
// with its effective settings. Settings a project inherits from the top level
// follow the reloaded file; the stored revisions are not read for anything
// else. Changed settings that need a restart keep cfg's values and are named:
// the listen socket, the web and tailnet listeners and the telemetry export.
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
	// The telemetry exporter is built once, at start.
	if next.Telemetry != cfg.Telemetry {
		restart = append(restart, "telemetry")
		pinned := *next
		pinned.Telemetry = cfg.Telemetry
		next = &pinned
	}
	return next, restart, nil
}

// effectiveProjects returns cfg's active projects as a reload of the top-level
// file top leaves them: their effective settings, with the one setting a
// project inherits, capacity.per_workstream, taken from top when the project's
// effective revision leaves it out. A project whose effective revision cannot
// be read again keeps its effective settings as they are.
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

// reloadError describes a configuration that failed to load by its file and
// field, never by values read from it.
func reloadError(err error, at time.Time) *ReloadError {
	var field *config.FieldError
	if errors.As(err, &field) {
		return &ReloadError{Path: field.Path, Field: field.Field, Message: field.Error(), At: at}
	}
	return &ReloadError{Message: "the configuration file cannot be located or read; check the top-level config.toml and its permissions", At: at}
}
