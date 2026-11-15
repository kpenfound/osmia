package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

// How often a project's gate worker looks for reached gates besides when a
// scheduling pass wakes it, and the least time between two looks.
var (
	gateRescan  = 2 * time.Second
	gateScanGap = 200 * time.Millisecond
)

// maxGateJudgments bounds the gates judged and applied at once, so a burst of
// gates does not send Jev unbounded concurrent requests. One gate that hangs
// holds only its own slot.
const maxGateJudgments = 32

// gateWorker decides the owner gates of one project for Jev. It runs beside
// the scheduler and never on its path: a scheduling pass only wakes it. It
// derives every gate that was reached and is not finished from the records, so
// the same pass that decides a new gate recovers one a restart interrupted:
//
//   - nothing recorded for the gate: Jev is asked once;
//   - a binding pick recorded, but no decision: the same pick is applied, with
//     no request;
//   - Jev's decision recorded but only partly carried out: carried out again;
//   - a nonbinding or fallback result, a refused pick, or a decision of the
//     owner: left alone, and never judged again.
//
// Each gate is judged and applied in its own goroutine.
type gateWorker struct {
	s      *Service
	active *activeProject
	wake   chan struct{}
	sem    chan struct{}
	wg     sync.WaitGroup
	mu     sync.Mutex
	busy   map[string]bool
}

func newGateWorker(s *Service, active *activeProject) *gateWorker {
	return &gateWorker{s: s, active: active, wake: make(chan struct{}, 1), sem: make(chan struct{}, maxGateJudgments), busy: map[string]bool{}}
}

// kick asks the worker to look for reached gates soon. It never waits.
func (w *gateWorker) kick() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// kickGates wakes the gate worker of the project whose trace is repository.
func (s *Service) kickGates(repository *trace.Repository) {
	if w, ok := s.gateWorkers.Load(repository); ok {
		w.(*gateWorker).kick()
	}
}

// run looks for reached gates until ctx ends, and then waits for the gates in
// flight, which end with it.
func (w *gateWorker) run(ctx context.Context) {
	defer w.wg.Wait()
	ticker := time.NewTicker(gateRescan)
	defer ticker.Stop()
	for {
		w.scan(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(gateScanGap):
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

// scan starts a goroutine for every reached gate that has none.
func (w *gateWorker) scan(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	gates, err := w.s.intake().ReachedGates(ctx, &intake.Project{ID: w.active.id, Repository: w.active.repository})
	if err != nil {
		if ctx.Err() == nil {
			w.s.log().Warn("cannot read the owner gates", "project", string(w.active.id), "error", err.Error())
		}
		return
	}
	for _, g := range gates {
		id := g.ID()
		w.mu.Lock()
		if w.busy[id] {
			w.mu.Unlock()
			continue
		}
		w.busy[id] = true
		w.mu.Unlock()
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			defer func() {
				w.mu.Lock()
				delete(w.busy, id)
				w.mu.Unlock()
			}()
			select {
			case w.sem <- struct{}{}:
				defer func() { <-w.sem }()
			case <-ctx.Done():
				return
			}
			w.handle(ctx, g)
		}()
	}
}

// handle brings one gate to where its records say it stands.
func (w *gateWorker) handle(ctx context.Context, g intake.Gate) {
	log := w.s.log().With("project", string(g.Project), "workstream", string(g.Stream), "gate", string(g.Kind), "subject", g.Key)
	repository := w.active.repository
	decision, err := g.Decision()
	if err != nil {
		log.Warn("cannot read the owner gate's decision", "error", err.Error())
		return
	}
	res, err := w.s.jev.Lookup(repository, g.Stream, g.Kind, g.Key)
	if err != nil {
		log.Warn("cannot read the owner gate's judgment", "error", err.Error())
		return
	}
	switch {
	case decision.Recorded && !(decision.ByJev && decision.Incomplete):
		// Decided by the owner, or by Jev and carried out.
		return
	case decision.Recorded:
		// Jev's decision is recorded and partly carried out.
		if res.Status == jev.Binding {
			w.apply(ctx, log, g, res)
		}
		return
	}
	switch res.Status {
	case jev.Binding:
		if res.Refused == "" {
			w.apply(ctx, log, g, res)
		}
		return
	case jev.Nonbinding, jev.FallbackOnly:
		if res.Status == jev.Nonbinding || res.Recorded {
			// The owner has the card: a below-threshold answer, a deferral or a
			// fallback is never judged again.
			return
		}
	}
	state, sources, err := g.State(ctx)
	if err != nil {
		log.Warn("cannot build the owner gate's state", "error", err.Error())
		return
	}
	res = w.s.jev.Decide(ctx, repository, jev.DecisionRequest{
		Scope: coreadapter.Scope{Project: string(g.Project), Workstream: string(g.Stream)}, Cause: g.Cause,
		Kind: g.Kind, Occurrence: g.Key, State: state, Options: g.Options, Sources: sources})
	if res.Status != jev.Binding {
		if ctx.Err() == nil {
			log.Info("owner gate goes to the owner", "judgment", res.Judgment, "status", string(res.Status), "reason", string(res.Reason))
		}
		return
	}
	// The judgment is recorded and the decision is not: an interruption here
	// leaves the pick for the next look to apply.
	if err := w.s.step("gate-judged:" + string(g.Kind)); err != nil {
		return
	}
	w.apply(ctx, log, g, res)
}

// applyAttempts bounds how often a gate's pick is tried against a transition
// that conflicts with a concurrent write, before the service's refusal stands.
const applyAttempts = 3

// apply carries Jev's binding pick out through the owner command's path, unless
// the owner decided the gate first. A refusal of the transition the pick calls
// for hands the gate to the owner with Jev's judgment shown, and is recorded on
// the judgment so the pick is not applied again, not even after a restart.
func (w *gateWorker) apply(ctx context.Context, log *slog.Logger, g intake.Gate, res jev.Result) {
	var api *intake.APIError
	for attempt := 1; attempt <= applyAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		decision, err := g.Decision()
		if err != nil {
			log.Warn("cannot read the owner gate's decision", "error", err.Error())
			return
		}
		if decision.Recorded && !(decision.ByJev && decision.Incomplete) {
			// The owner decided first, or the decision is carried out.
			return
		}
		if api = g.Apply(ctx, res); api == nil {
			log.Info("owner gate decided by Jev", "judgment", res.Judgment, "pick", res.Pick, "confidence", res.Confidence)
			return
		}
		if api.Code != intake.Conflict {
			break
		}
	}
	if ctx.Err() != nil {
		return
	}
	switch api.Code {
	case intake.Internal, intake.Unavailable:
		// The record could not be read or written now. The pick stays pending
		// and the next look applies it.
		log.Warn("owner gate's pick could not be applied now", "judgment", res.Judgment, "error", api.Message)
		return
	}
	if decision, err := g.Decision(); err == nil && decision.Recorded && !decision.ByJev {
		return
	}
	reason := api.Message
	if err := w.s.jev.Refuse(ctx, w.active.repository, g.Stream, res.Judgment, reason); err != nil {
		log.Warn("cannot record the refusal of the owner gate's pick", "judgment", res.Judgment, "error", err.Error())
		return
	}
	log.Info("owner gate's pick refused; the decision is the owner's", "judgment", res.Judgment, "pick", res.Pick, "reason", reason)
}
