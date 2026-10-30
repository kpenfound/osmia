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
	for _, p := range cfg.Projects {
		if !next.Active(p.ID) {
			if api := s.projectRemovalGate(p.ID); api != nil {
				return ReloadResponse{}, api
			}
		}
	}
	unchanged := &APIError{Code: Internal, Message: "cannot apply the reloaded configuration to the running project; the loaded configuration is unchanged"}
	failUnchanged := func() (ReloadResponse, *APIError) {
		s.log().Error("reload failed", "error", unchanged.Message)
		return ReloadResponse{}, unchanged
	}
	staged := map[*activeProject]*stages{}
	var added, removed []*activeProject
	rollback := func() {
		for _, p := range added {
			s.pool.Leave(p.id)
			p.repository.Close()
		}
	}
	for _, p := range projects {
		if !next.Active(p.id) {
			removed = append(removed, p)
			continue
		}
		if staged[p], err = s.stages(next.For(p.id), p.repository); err != nil {
			return failUnchanged()
		}
	}
	for _, id := range next.ProjectIDs() {
		present := false
		for _, p := range projects {
			present = present || p.id == id
		}
		if !present {
			p, err := s.open(next.For(id))
			if err != nil {
				rollback()
				return failUnchanged()
			}
			if p != nil {
				added = append(added, p)
			}
		}
	}
	all := append(projects, added...)
	projects = nil
	for _, id := range next.ProjectIDs() {
		for _, p := range all {
			if p.id == id {
				projects = append(projects, p)
			}
		}
	}
	projects = append(projects, removed...)
	if err := s.ensureEngine(context.Background(), next); err != nil {
		rollback()
		return failUnchanged()
	}
	s.mu.Lock()
	if err := s.resolveRuntime(next, projects); err != nil {
		s.mu.Unlock()
		rollback()
		return failUnchanged()
	}
	s.cfg, s.projects, s.reloadErr = next, projects, nil
	s.checkSlots.resize(next.Capacity.Checks)
	for p, stage := range staged {
		p.pipeline.next.Store(stage)
		p.pipeline.conversation.Store(stage)
	}
	for _, p := range removed {
		s.log().Info("project draining", "project", string(p.id), "reason", "reload")
		p.controller.Drain()
		p.conversations.Drain()
		if p.pollCancel != nil {
			p.pollCancel()
		}
	}
	for _, p := range added {
		s.launch(p)
	}
	s.mu.Unlock()
	for _, p := range removed {
		if err := s.finishDraining(p); err != nil {
			s.log().Error("project stopped", "project", string(p.id), "reason", "reload", "error", err.Error())
			return ReloadResponse{}, &APIError{Code: Internal, Message: "configuration applied but a removed project could not finish draining; restart the service"}
		}
		s.log().Info("project stopped", "project", string(p.id), "reason", "reload")
	}
	// The runtime's effective profiles and the daily budget's limit follow the
	// configuration.
	events := []Event{{Kind: EventResync}, {Kind: EventConfig}, {Kind: EventRuntime}}
	for _, id := range next.ProjectIDs() {
		events = append(events, Event{Kind: EventSpend, Project: id})
	}
	if len(next.Projects) == 0 {
		events = append(events, Event{Kind: EventSpend})
	}
	s.hub.Publish(events...)
	s.log().Info("reload succeeded", "digest", digest(next), "restart_required", len(restart) > 0, "added", len(added), "removed", len(removed))
	return ReloadResponse{Digest: digest(next), RestartRequired: restart}, nil
}

// candidate loads the configuration on disk as a reload applies it over cfg.
// Changed settings that need a restart keep cfg's values and are named: the
// listen socket and the web and tailnet listeners.
func (s *Service) candidate(cfg *config.Config) (*config.Config, []string, error) {
	next, err := config.Load(s.options.Config)
	if err != nil {
		return nil, nil, err
	}
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
	return next, restart, nil
}

// reloadError describes a configuration that failed to load by its file and
// field, never by values read from it.
func reloadError(err error, at time.Time) *ReloadError {
	var field *config.FieldError
	if errors.As(err, &field) {
		return &ReloadError{Path: field.Path, Field: field.Field, Message: field.Error(), At: at}
	}
	return &ReloadError{Message: "the configuration files cannot be located or read; check the top-level config.toml, the project files under the root and their permissions", At: at}
}
