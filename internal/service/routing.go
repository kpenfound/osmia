package service

import (
	"context"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/recordspans"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) newRouter() *router.Router {
	effects := &router.Effects{}
	execution.RegisterUnitEffects(effects, s.execution)
	lifecycle.RegisterRoutingEffects(effects, s.lifecycle)
	intake.RegisterObjectionEffects(effects, s.intake)
	intake.RegisterWorkstreamEffects(effects, s.intake)
	lifecycle.RegisterWorkstreamEffects(effects, s.lifecycle)
	rebuild := &router.Rebuilders{}
	rebuild.Register(execution.UnitMachine, execution.RebuildUnitExit(s.execution))
	rebuild.Register(intake.WorkstreamMachine, lifecycle.RebuildWorkstreamExit(s.lifecycle))
	return &router.Router{
		Judge:   s.jev,
		Stops:   s.routeStops,
		Paused:  s.routePaused,
		Effects: effects,
		Rebuild: rebuild,
		Spans: func(r router.SpanRoute) {
			s.recorder().Route(recordspans.Route{Project: r.Project, Workstream: r.Workstream, Unit: r.Unit, Occurrence: r.Occurrence, Trigger: r.Trigger,
				Default: r.Default, Chosen: r.Chosen, Outcome: r.Outcome, Confidence: r.Confidence, Judgment: r.Judgment, Start: r.Start, End: r.End})
		},
		Now: s.now,
		Log: s.log(),
	}
}

// Router returns the transition router. Controllers register the side effects
// of their targets on its Effects.
func (s *Service) Router() *router.Router { return s.router }

// RouteExit decides an exit and commits the result, waiting in place for Jev up
// to its timeout. It is for callers that already wait on a request, such as an
// owner command or a chief-of-staff tool, and never for a scheduling pass.
func (s *Service) RouteExit(ctx context.Context, x router.Exit) (router.Result, error) {
	return s.router.Route(ctx, x)
}

// SubmitExit hands an exit raised on the scheduling path to the routing worker
// and returns at once; done, when set, receives the result. It reports false
// when the occurrence is already being routed.
func (s *Service) SubmitExit(x router.Exit, done func(router.Result, error)) bool {
	if s.routing == nil {
		return false
	}
	return s.routing.Submit(x, done)
}

type routerPort struct{ s *Service }

func (p routerPort) Route(ctx context.Context, x router.Exit) (router.Result, error) {
	return p.s.RouteExit(ctx, x)
}

func (p routerPort) Admit(ctx context.Context, x router.Exit) (router.Admission, error) {
	return p.s.router.Admit(ctx, x)
}

func (p routerPort) Submit(x router.Exit, done func(router.Result, error)) bool {
	return p.s.SubmitExit(x, done)
}

func (s *Service) recoverRoutes(ctx context.Context, repository *trace.Repository) {
	streams, err := repository.Workstreams()
	if err != nil {
		s.log().Warn("cannot list the workstreams to recover transition picks", "error", err.Error())
		return
	}
	for _, stream := range streams {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.router.Recover(ctx, repository, stream); err != nil && ctx.Err() == nil {
			s.log().Warn("cannot finish a recorded transition pick", "workstream", string(stream), "error", err.Error())
		}
	}
}
