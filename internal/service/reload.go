package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
//
// Once draining is set, a pass runs no hook, so the project dispatches and
// requests nothing new, while the operations already recorded are reconciled
// as before. The first pass that finds drained true ends the loop with
// errDrained.
type pipeline struct {
	current, next atomic.Pointer[stages]
	draining      atomic.Bool
	drained       func() (bool, error)
}

// errDrained ends the loop of a draining project that has no operation left
// to reconcile.
var errDrained = errors.New("project drained")

func (p *pipeline) schedule(ctx context.Context) error {
	if p.draining.Load() {
		done, err := p.drained()
		if err != nil {
			return fmt.Errorf("drain pass: %w", err)
		}
		if done {
			return errDrained
		}
		return nil
	}
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

// settled reports whether every operation of the trace is acknowledged or
// held by hold, so none is in flight or waiting to be reconciled.
func settled(repository *trace.Repository, hold func(config.WorkstreamID, coreadapter.Operation) bool) (bool, error) {
	streams, err := repository.Workstreams()
	if err != nil {
		return false, err
	}
	for _, stream := range streams {
		records, err := repository.Operations(stream)
		if err != nil {
			return false, err
		}
		for _, record := range records {
			if !record.Acknowledged && (hold == nil || !hold(stream, record.Operation)) {
				return false, nil
			}
		}
	}
	return true, nil
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

// reload reads the top-level configuration and every listed project's file as
// one candidate and replaces the loaded configuration with it only when all of
// them pass. A failure is kept as the last reload error until a reload
// succeeds. Settings that need a restart keep their loaded values and are
// named in the response. The runtime state is resolved against the
// candidate, never rewritten, and the running projects' passes adopt the
// candidate's stages from their next pass. A project the candidate adds to
// active_projects opens and starts as at startup; one it leaves out drains
// as project remove drains it. Listing a project that is still draining is
// refused.
func (s *Service) reload(ctx context.Context) (ReloadResponse, *APIError) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	cfg, projects := s.runtimes()
	next, restart, err := s.candidate(cfg)
	if err == nil {
		for _, p := range s.drainingProjects() {
			if next.Active(p.ID) {
				path, _ := cfg.Root.Config()
				err = &config.FieldError{Path: path, Field: "active_projects", Reason: fmt.Sprintf("project %s is still draining; list it again once status no longer shows it draining", p.ID)}
			}
		}
	}
	if err != nil {
		failed := reloadError(err, s.now())
		s.mu.Lock()
		s.reloadErr = failed
		s.mu.Unlock()
		s.hub.publish(Event{Kind: EventConfig})
		return ReloadResponse{}, &APIError{Validation, failed.Message + "; the loaded configuration is unchanged"}
	}
	unchanged := &APIError{Internal, "cannot apply the reloaded configuration to the running projects; the loaded configuration is unchanged"}
	var kept, removed []*activeProject
	var staged []*stages
	for _, p := range projects {
		if !next.Active(p.id) {
			removed = append(removed, p)
			continue
		}
		st, err := s.stages(next.For(p.id), p.repository)
		if err != nil {
			return ReloadResponse{}, unchanged
		}
		kept, staged = append(kept, p), append(staged, st)
	}
	var added []*activeProject
	closeAdded := func() {
		for _, p := range added {
			p.repository.Close()
		}
	}
	for _, id := range next.ProjectIDs() {
		if cfg.Active(id) {
			continue
		}
		p, err := s.openRecovered(ctx, next.For(id))
		if err != nil {
			closeAdded()
			return ReloadResponse{}, unchanged
		}
		if p != nil {
			added = append(added, p)
		}
	}
	// The running projects keep active_projects order.
	running := slices.Concat(kept, added)
	order := next.ProjectIDs()
	slices.SortStableFunc(running, func(a, b *activeProject) int {
		return slices.Index(order, a.id) - slices.Index(order, b.id)
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.resolveRuntime(next, running); err != nil {
		closeAdded()
		return ReloadResponse{}, unchanged
	}
	s.cfg, s.projects, s.reloadErr = next, running, nil
	for i, p := range kept {
		p.pipeline.next.Store(staged[i])
	}
	for _, p := range added {
		s.launch(p)
	}
	for _, p := range removed {
		s.drain(p, cfg.For(p.id).Project)
	}
	// The runtime's effective profiles and the daily budget's limit follow the
	// configuration.
	events := []Event{{Kind: EventConfig}, {Kind: EventRuntime}}
	for _, id := range next.ProjectIDs() {
		events = append(events, Event{Kind: EventSpend, Project: id})
	}
	if len(next.Projects) == 0 {
		events = append(events, Event{Kind: EventSpend})
	}
	if len(added) > 0 || len(removed) > 0 || !slices.Equal(next.ProjectIDs(), cfg.ProjectIDs()) {
		events = append(events, Event{Kind: EventResync})
	}
	s.hub.publish(events...)
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
