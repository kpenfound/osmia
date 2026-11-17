package router

import (
	"context"
	"sync"
)

// maxRouting bounds the exits a worker routes at once, so a burst of exits does
// not send Jev unbounded concurrent requests. One exit that waits on Jev holds
// only its own slot.
const maxRouting = 32

// Worker routes the exits a scheduling pass raises. A pass never waits on Jev:
// it submits its exit and goes on, and the worker routes it beside the
// scheduler, as the gate worker decides the owner gates. Nothing is queued
// across a restart; the pass that raised an exit raises it again while the
// subject is still where the exit left it, and the router finishes whatever
// the earlier run recorded.
type Worker struct {
	Router *Router

	ctx  context.Context
	sem  chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex
	busy map[string]bool
}

// NewWorker returns a worker whose exits are routed until ctx ends.
func NewWorker(ctx context.Context, r *Router) *Worker {
	return &Worker{Router: r, ctx: ctx, sem: make(chan struct{}, maxRouting), busy: map[string]bool{}}
}

// Submit hands an exit to the worker and returns at once. It reports false when
// the exit's occurrence is already being routed. done, when set, is called with
// the result once the exit is decided; a failure to read or write is passed
// to it and the exit stays for the caller's next pass.
func (w *Worker) Submit(x Exit, done func(Result, error)) bool {
	key := w.id(x)
	w.mu.Lock()
	if w.busy[key] || w.ctx.Err() != nil {
		w.mu.Unlock()
		return false
	}
	w.busy[key] = true
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			w.mu.Lock()
			delete(w.busy, key)
			w.mu.Unlock()
		}()
		select {
		case w.sem <- struct{}{}:
			defer func() { <-w.sem }()
		case <-w.ctx.Done():
			return
		}
		res, err := w.Router.Route(w.ctx, x)
		if err != nil && w.ctx.Err() == nil {
			w.Router.logger().Warn("an exit could not be routed; its next pass routes it again", "workstream", string(x.Workstream), "subject", x.Subject, "trigger", x.Trigger, "error", err.Error())
		}
		if done != nil {
			done(res, err)
		}
	}()
	return true
}

func (w *Worker) id(x Exit) string {
	return string(x.Workstream) + "/" + OccurrenceKey(x)
}

// Wait returns once every exit submitted so far is routed or abandoned. A
// worker whose context ended abandons the exits it has not started.
func (w *Worker) Wait() { w.wg.Wait() }
