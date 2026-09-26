// Package reconcile runs level-triggered controllers for durable local operations.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

type Options struct {
	Worker     string
	Adapters   map[coreadapter.OperationBoundary]coreadapter.Reconciler
	Now        func() time.Time
	Ticks      <-chan time.Time
	Interval   time.Duration
	RetryDelay time.Duration
	// Schedule runs at the start of every pass, before operations are read, so
	// the intent it publishes is reconciled in the same pass. Its failure stops
	// the loop.
	Schedule func(context.Context) error
	// Priority orders the pending operations of a pass across workstreams: every
	// operation of a lower value is reconciled before any of a higher one.
	// Without it a pass goes workstream by workstream.
	Priority func(coreadapter.Operation) int
	// Hold reports whether a due operation of the workstream waits: a held
	// operation is neither claimed nor recorded in the pass, and is reconciled
	// by the first pass that no longer holds it.
	Hold func(config.WorkstreamID, coreadapter.Operation) bool
	// Concurrent reports whether a due operation is reconciled beside the
	// pass: the pass starts it and goes on, and its effect runs without the
	// repository's operation lock, so the pass's other operations and later
	// passes proceed while it is in flight. A later pass leaves an operation
	// in flight alone. Its failure stops the loop, as a store failure of the
	// pass does. Without it every operation is reconciled in the pass.
	Concurrent func(coreadapter.Operation) bool
}

type Controller struct {
	repository *trace.Repository
	options    Options
	boundary   func(string) error

	mu sync.Mutex
	// running holds the concurrent operations in flight, by workstream and
	// event; group joins them.
	running map[string]bool
	group   sync.WaitGroup
	// failure is the first error a concurrent operation returned, and abort
	// cancels the context Run passes to them.
	failure error
	abort   context.CancelCauseFunc
}

func New(repository *trace.Repository, options Options) (*Controller, error) {
	if repository == nil || options.Worker == "" || options.Interval < 0 || options.RetryDelay < 0 {
		return nil, fmt.Errorf("invalid reconciliation options")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Interval == 0 {
		options.Interval = time.Second
	}
	if options.RetryDelay == 0 {
		options.RetryDelay = time.Second
	}
	adapters := make(map[coreadapter.OperationBoundary]coreadapter.Reconciler)
	for k, v := range options.Adapters {
		adapters[k] = v
	}
	options.Adapters = adapters
	return &Controller{repository: repository, options: options, running: map[string]bool{}}, nil
}

// Run scans before waiting, then after every coalesced hint or periodic tick.
// Store failures stop the loop; adapter failures are durably scheduled for retry.
// Run cancels the concurrent operations in flight and joins them before it
// returns.
func (c *Controller) Run(ctx context.Context) error {
	ticks := c.options.Ticks
	if ticks == nil {
		ticker := time.NewTicker(c.options.Interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	ctx, cancel := context.WithCancelCause(ctx)
	c.mu.Lock()
	c.abort = cancel
	c.mu.Unlock()
	err := c.loop(ctx, ticks)
	cancel(err)
	if failure := c.Wait(); failure != nil {
		return failure
	}
	return err
}

func (c *Controller) loop(ctx context.Context, ticks <-chan time.Time) error {
	for {
		if err := c.Pass(ctx); err != nil {
			return err
		}
		if err := c.repository.WaitWorkflow(ctx, ticks); err != nil {
			return err
		}
	}
}

// Wait joins the concurrent operations in flight and returns the first error
// one of them returned while its context was live.
func (c *Controller) Wait() error {
	c.group.Wait()
	return c.failed()
}

func (c *Controller) failed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failure
}

func (c *Controller) inFlight(stream config.WorkstreamID, event string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running[string(stream)+"/"+event]
}

// start reconciles the operation in a goroutine of its own, its effect
// outside the repository's operation lock.
func (c *Controller) start(ctx context.Context, stream config.WorkstreamID, event string) {
	key := string(stream) + "/" + event
	c.mu.Lock()
	c.running[key] = true
	c.mu.Unlock()
	c.group.Add(1)
	go func() {
		defer c.group.Done()
		err := c.repository.WithOperation(ctx, stream, event, trace.Actor{Kind: "service", ID: c.options.Worker}, c.options.Now, func(attempt *trace.OperationAttempt, current trace.OperationRecord) error {
			return c.reconcile(ctx, attempt, current, true)
		})
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.running, key)
		// An operation cut short by cancellation is reconciled after restart.
		if err != nil && ctx.Err() == nil && c.failure == nil {
			c.failure = err
			if c.abort != nil {
				c.abort(err)
			}
		}
	}()
}

func (c *Controller) step(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.boundary != nil {
		return c.boundary(name)
	}
	return nil
}

// Pass discovers work from manifests and durable intent; no wakeup payload is used.
func (c *Controller) Pass(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.failed(); err != nil {
		return err
	}
	if c.options.Schedule != nil {
		if err := c.options.Schedule(ctx); err != nil {
			return err
		}
	}
	streams, err := c.repository.Workstreams()
	if err != nil {
		return err
	}
	type pending struct {
		stream config.WorkstreamID
		record trace.OperationRecord
	}
	var due []pending
	for _, stream := range streams {
		records, err := c.repository.Operations(stream)
		if err != nil {
			return err
		}
		for _, record := range records {
			if !record.Acknowledged && !c.options.Now().Before(record.RetryAt) && !c.inFlight(stream, record.EventID) && (c.options.Hold == nil || !c.options.Hold(stream, record.Operation)) {
				due = append(due, pending{stream, record})
			}
		}
	}
	if c.options.Priority != nil {
		slices.SortStableFunc(due, func(a, b pending) int {
			return c.options.Priority(a.record.Operation) - c.options.Priority(b.record.Operation)
		})
	}
	for _, p := range due {
		if err := c.step(ctx, "before-claim"); err != nil {
			return err
		}
		if c.options.Concurrent != nil && c.options.Concurrent(p.record.Operation) {
			c.start(ctx, p.stream, p.record.EventID)
			continue
		}
		err := c.repository.WithOperation(ctx, p.stream, p.record.EventID, trace.Actor{Kind: "service", ID: c.options.Worker}, c.options.Now, func(attempt *trace.OperationAttempt, current trace.OperationRecord) error {
			return c.reconcile(ctx, attempt, current, false)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// reconcile claims, inspects and completes one operation. A concurrent
// operation applies its effect outside the repository's operation lock.
func (c *Controller) reconcile(ctx context.Context, attempt *trace.OperationAttempt, record trace.OperationRecord, concurrent bool) error {
	if err := c.step(ctx, "after-claim"); err != nil {
		return err
	}
	write := func(kind string) error { return attempt.Record(ctx, attempt.Action(kind, c.options.Now())) }
	retry := func(reason string) error {
		action := attempt.Action("retry", c.options.Now())
		action.Failure, action.RetryAt = reason, action.At.Add(c.options.RetryDelay)
		if err := attempt.Record(ctx, action); err != nil {
			return err
		}
		return c.step(ctx, "after-retry")
	}
	if record.Result == nil {
		if err := c.step(ctx, "before-inspection"); err != nil {
			return err
		}
		adapter := c.options.Adapters[record.Operation.Boundary]
		observed := coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "No reconciliation adapter configured"}
		var err error
		if adapter != nil {
			observed, err = adapter.Inspect(ctx, record.Operation)
		}
		if err != nil {
			observed = coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: fmt.Sprintf("Inspection failed: %v; evidence: %s", err, observed.Evidence)}
		}
		if err := c.step(ctx, "after-inspection"); err != nil {
			return err
		}
		action := attempt.Action("observe", c.options.Now())
		action.Observation = &observed
		if err := attempt.Record(ctx, action); err != nil {
			return err
		}
		if err := c.step(ctx, "after-observation"); err != nil {
			return err
		}
		var result coreadapter.OperationResult
		switch observed.State {
		case coreadapter.EffectCompleted:
			result = *observed.Result
		case coreadapter.EffectAbsent:
			if err := write("effect"); err != nil {
				return err
			}
			if err := c.step(ctx, "before-effect"); err != nil {
				return err
			}
			apply := func() { result, err = adapter.Apply(ctx, record.Operation) }
			if concurrent {
				attempt.Unlocked(apply)
			} else {
				apply()
			}
			if e := c.step(ctx, "after-effect"); e != nil {
				return e
			}
			if err != nil {
				return retry(fmt.Sprintf("Effect returned error: %v; evidence: %s", err, result.Evidence))
			}
		case coreadapter.EffectUnknown:
			return retry(observed.Evidence)
		default:
			return errors.New("invalid inspection state")
		}
		if err := c.step(ctx, "before-result"); err != nil {
			return err
		}
		action = attempt.Action("result", c.options.Now())
		action.Result = &result
		if err := attempt.Record(ctx, action); err != nil {
			return err
		}
		if err := c.step(ctx, "after-result"); err != nil {
			return err
		}
	}
	if err := c.step(ctx, "before-acknowledgement"); err != nil {
		return err
	}
	if err := write("acknowledge"); err != nil {
		return err
	}
	return c.step(ctx, "after-acknowledgement")
}
