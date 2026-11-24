package execution

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
)

type inlineRouter struct{ r *router.Router }

func (i inlineRouter) Route(ctx context.Context, x router.Exit) (router.Result, error) {
	return i.r.Route(ctx, x)
}

func (i inlineRouter) Submit(x router.Exit, done func(router.Result, error)) bool {
	res, err := i.r.Route(context.Background(), x)
	if done != nil {
		done(res, err)
	}
	return true
}

func routeWith(c *Controller, now func() time.Time, p *jevfixture.Transitions) *router.Router {
	effects := &router.Effects{}
	RegisterUnitEffects(effects, func() *Controller { return c })
	r := &router.Router{Judge: jevfixture.TransitionJudge(now, p), Effects: effects, Now: now}
	c.Router = inlineRouter{r}
	return r
}

type workerRouter struct {
	r *router.Router
	w *router.Worker
}

func (p workerRouter) Route(ctx context.Context, x router.Exit) (router.Result, error) {
	return p.r.Route(ctx, x)
}

func (p workerRouter) Submit(x router.Exit, done func(router.Result, error)) bool {
	return p.w.Submit(x, done)
}
