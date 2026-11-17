package execution

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/router"
)

// inlineRouter is the router as a fixture controller uses it: a pass the test
// runs directly is no scheduling pass, so its exits are routed in place.
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

// routeWith routes the exits of c through a router whose Jev answers as p does.
func routeWith(c *Controller, now func() time.Time, p *jevfixture.Transitions) *router.Router {
	effects := &router.Effects{}
	RegisterUnitEffects(effects, func() *Controller { return c })
	r := &router.Router{Judge: jevfixture.TransitionJudge(now, p), Effects: effects, Now: now}
	c.Router = inlineRouter{r}
	return r
}

// workerRouter is the router as the service gives it to the controllers: an
// exit a scheduling pass raises goes to the worker.
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
