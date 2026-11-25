package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// climbConcludeHost is the machinery a concluding Hill Climb workstream's one
// rebase runs on: the climb's clone, the upstream base the foreman resolves
// for every workstream, and the replay and move the foreman's own rebase of a
// feature branch uses.
type climbConcludeHost struct {
	s          *Service
	cfg        *config.Config
	repository *trace.Repository
	stream     config.WorkstreamID
	g          workspace.Provider
}

var _ climb.ConcludeHost = (*climbConcludeHost)(nil)

func (h *climbConcludeHost) Export(ctx context.Context, commit, dir string) error {
	return h.g.Export(ctx, commit, dir)
}

func (h *climbConcludeHost) Branch(ctx context.Context, name string) (string, bool, error) {
	return h.g.Branch(ctx, name)
}

func (h *climbConcludeHost) Target(ctx context.Context, tip string) (climb.Target, string, error) {
	upstream, err := h.s.resolveBase(ctx, h.cfg, h.repository, h.stream, h.g)
	if err != nil {
		return climb.Target{}, "", err
	}
	current, err := lifecycle.BranchBase(ctx, h.repository, h.stream, h.g, tip)
	if err != nil {
		return climb.Target{}, "", err
	}
	return climb.Target{Workstream: upstream.Workstream, Remote: upstream.Remote, Branch: upstream.Branch, Commit: upstream.Commit}, current.Commit, nil
}

func (h *climbConcludeHost) Replay(ctx context.Context, base, before, onto string, at time.Time) (string, []string, error) {
	var commit string
	var conflicts []string
	err := workspace.LocalStep(ctx, string(h.stream)+"/conclusion/replay", at, []workspace.Provider{h.g}, func(local context.Context) error {
		var err error
		commit, conflicts, err = h.g.ReplayFrom(local, base, before, onto, at)
		return err
	})
	return commit, conflicts, err
}

func (h *climbConcludeHost) Move(ctx context.Context, key, from, to string) error {
	branch := intake.FeatureBranch(h.stream)
	return workspace.LocalStep(ctx, key+"/move", h.s.now(), []workspace.Provider{h.g}, func(local context.Context) error {
		acquired, err := h.g.Acquire(local, vcs.Request{Name: string(h.stream), Branch: branch})
		if err != nil {
			return fmt.Errorf("climb branch %s: %w", branch, err)
		}
		return h.g.Move(local, acquired.(workspace.Worktree), from, to)
	})
}

// climbConcluder returns the concluder of the project's Hill Climb
// workstreams, bound to the project's configuration as it is now.
func (s *Service) climbConcluder(repository *trace.Repository) *climb.Concluder {
	cfg := s.about(repository)
	root := filepath.Join(cfg.Root.String(), "climb", string(repository.Project()))
	return &climb.Concluder{
		Repository: repository,
		Spec: func(stream config.WorkstreamID) (climb.Spec, int, error) {
			spec, sealed, err := intake.SealedClimbSpec(repository, cfg, stream)
			return spec, sealed.Revision.Spec, err
		},
		Branch: intake.FeatureBranch,
		Host: func(stream config.WorkstreamID) (climb.ConcludeHost, error) {
			g, err := intake.FeatureWorkspaces(cfg, repository).Of(stream)
			if err != nil {
				return nil, err
			}
			return &climbConcludeHost{s: s, cfg: cfg, repository: repository, stream: stream, g: g}, nil
		},
		Runner: func(stream config.WorkstreamID, source climb.Source) (climb.Runner, error) {
			engine, ok := s.configuredChecks(cfg).(checks.FunctionCaller)
			if !ok {
				return climb.Runner{}, errors.New("the configured check client cannot call a function of the dev module")
			}
			project := repository.Project()
			return climb.Runner{Source: source, Engine: engine, Root: filepath.Join(root, "measure"), Timeout: cfg.HillClimb().MeasurementTimeout,
				AcquireSlot: func(ctx context.Context) (func(), error) {
					return s.checkSlots.acquire(ctx, CheckSlotRun{Project: project, Workstream: stream, Unit: "conclude"})
				}}, nil
		},
		Publish: func(kind api.EventKind, stream config.WorkstreamID) {
			s.hub.Publish(api.Event{Kind: kind, Project: repository.Project(), Workstream: stream})
		},
		Now:  s.now,
		Step: s.step,
	}
}
