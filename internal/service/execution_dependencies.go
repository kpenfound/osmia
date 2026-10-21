package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) execution() *execution.Controller {
	return execution.New(execution.Dependencies{
		RefreshPendingStreams: refreshPendingStreams,
		RefreshBudget: func(ctx context.Context, r *trace.Repository) error {
			return (dailyBudget{s: s, repository: r}).Pass(ctx)
		},
		AcquireCheckSlot: func(ctx context.Context, p config.ProjectID, w config.WorkstreamID, u string) (func(), error) {
			return s.checkSlots.acquire(ctx, CheckSlotRun{Project: p, Workstream: w, Unit: u})
		},
		Now: s.now, Effective: s.effective, RoleExecution: s.roleExecution, ContextFor: s.contextFor,
		Step: s.step, About: s.about, ConversationTrace: s.conversationTrace, Checks: s.options.reviewChecks, Jev: s.jev,
		Runtimes: func() (*config.Config, []*execution.Project) {
			cfg, projects := s.runtimes()
			out := make([]*execution.Project, 0, len(projects))
			for _, p := range projects {
				out = append(out, &execution.Project{Repository: p.repository})
			}
			return cfg, out
		},
		RefreshPending: refreshPending, EdgeWaiting: edgeWaiting, ChangeHeld: changeHeld, ChangeRepositoryHeld: changeRepositoryHeld,
		WorkgroupEdgePending: workgroupEdgePending, CarryBase: func(r *trace.Repository, w config.WorkstreamID, onto string) (string, bool, error) {
			d, found, err := carryDrift(r, w, onto)
			return d.Before, found, err
		},
		CarriedOnto: carriedOnto, UnitDiscoveries: unitDiscoveries, UnitDrift: unitDrift, UnitRebaseConflicts: unitRebaseConflicts,
	})
}
func (c *runtimeControls) execution() *execution.Controls {
	return &execution.Controls{Load: func() *execution.Controller {
		if c == nil {
			return nil
		}
		s := c.service.Load()
		if s == nil {
			return nil
		}
		return s.execution()
	}}
}
func (c *runtimeControls) moveUnit(r *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return c.execution().MoveUnit(r, scope)
}
func (c *runtimeControls) resolveContested(r *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	return c.execution().ResolveContested(r, scope)
}

func (s *Service) ownerMove(ctx context.Context, raw, unit string, req UnitMoveRequest) (UnitMoveResponse, *APIError) {
	return s.execution().OwnerMove(ctx, raw, unit, req)
}

func (s *Service) ruleContested(ctx context.Context, rawStream, unit string, req ContestedRulingRequest) (ContestedRulingResponse, *APIError) {
	return s.execution().RuleContested(ctx, rawStream, unit, req)
}

var captureFiles = execution.CaptureFiles

var unitName = execution.UnitName

func (m *masons) recoverView(ctx context.Context, stream config.WorkstreamID, agent string, view func() (string, error)) error {
	return m.driver().RecoverView(ctx, stream, agent, view)
}
