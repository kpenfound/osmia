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
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/trace"
)

// turnAction mirrors thread.TurnAction, the runner-boundary action that
// delivers one queued turn. The controller logs those separately, with the
// role and profile only the service's turn adapter knows, so it leaves them
// out of its own generic operation records to keep the log readable.
const turnAction = "thread-turn"

type Options struct {
	// Workstreams supplies an explicit operation scope when the repository
	// aggregate omits it, such as a shared chief's synthetic host.
	// Nil scans Repository.Workstreams.
	Workstreams func() ([]config.WorkstreamID, error)
	Worker      string
	Adapters    map[coreadapter.OperationBoundary]coreadapter.Reconciler
	Now         func() time.Time
	Ticks       <-chan time.Time
	Interval    time.Duration
	// RetryDelay is the wait after an operation's first failed attempt. Each
	// further consecutive failure doubles it, up to MaxRetryDelay, which
	// defaults to DefaultMaxRetryDelay or RetryDelay, whichever is longer.
	RetryDelay    time.Duration
	MaxRetryDelay time.Duration
	// Schedule runs at the start of every pass before operations are read.
	// Repository writes take their own short bookkeeping locks. Scheduling
	// never excludes operation claims or result writes. Failures are reported
	// and retried on the next wakeup or tick without admitting more operations
	// from that pass.
	Schedule func(context.Context) error
	// Priority orders the pending operations of a pass across workstreams: every
	// operation of a lower value is reconciled before any of a higher one.
	// Without it a pass goes workstream by workstream.
	Priority func(coreadapter.Operation) int
	// Hold reports whether a due operation of the workstream waits: a held
	// operation is neither claimed nor recorded in the pass, and is reconciled
	// by the first pass that no longer holds it.
	Hold func(config.WorkstreamID, coreadapter.Operation) bool
	// Concurrent runs an operation beside the pass, including inspection and
	// result recording. Admission claims in priority order without waiting for
	// inspection, an effect, a workstream reservation, or a limit permit.
	Concurrent func(coreadapter.Operation) bool
	// Class sets ordering independently of Concurrent. When absent, Concurrent
	// operations are workers and inline operations are workflows. Conversation
	// operations rely only on their thread's ordering.
	Class func(coreadapter.Operation) trace.OperationClass
	// Limit tries to acquire capacity for an operation's kind. It must return
	// immediately. False leaves the operation pending; true supplies an optional
	// release called after the attempt finishes (also on skipped admission).
	// Several kinds can share a pool, including with callers outside this loop.
	// External pool users call Repository.WakeWorkflow when returning capacity.
	Limit func(coreadapter.Operation) (release func(), admitted bool)
	// SlowThreshold sets operation and bookkeeping lock-wait diagnostics.
	// Zero uses trace.SlowThreshold.
	SlowThreshold time.Duration
}

// DefaultMaxRetryDelay is the longest wait between attempts of a failing
// operation when Options.MaxRetryDelay is unset.
const DefaultMaxRetryDelay = 5 * time.Minute

// RetryAction is the kind of the operation action that records a failed
// attempt and when the next one is due.
const RetryAction = "retry"

// ConsecutiveFailures counts the failed attempts the operation recorded.
// Every failure of an operation is consecutive: a result ends its attempts.
func ConsecutiveFailures(record trace.OperationRecord) int {
	n := 0
	for _, action := range record.History {
		if action.Kind == RetryAction {
			n++
		}
	}
	return n
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
	// failure is the first terminal operation store error. abort cancels
	// the context Run passes to operations beside the pass.
	failure   error
	abort     context.CancelCauseFunc
	drain     chan struct{}
	drainOnce sync.Once
}

func New(repository *trace.Repository, options Options) (*Controller, error) {
	if repository == nil || options.Worker == "" || options.Interval < 0 || options.RetryDelay < 0 || options.MaxRetryDelay < 0 || options.SlowThreshold < 0 {
		return nil, fmt.Errorf("invalid reconciliation options")
	}
	if options.SlowThreshold == 0 {
		options.SlowThreshold = trace.SlowThreshold
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
	if options.MaxRetryDelay == 0 {
		options.MaxRetryDelay = max(DefaultMaxRetryDelay, options.RetryDelay)
	}
	adapters := make(map[coreadapter.OperationBoundary]coreadapter.Reconciler)
	for k, v := range options.Adapters {
		adapters[k] = v
	}
	options.Adapters = adapters
	return &Controller{repository: repository, options: options, running: map[string]bool{}, drain: make(chan struct{})}, nil
}

// Drain stops admitting operations and lets operations already in flight
// finish. Cancellation of Run still interrupts those operations.
func (c *Controller) Drain() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drainOnce.Do(func() { close(c.drain) })
}

func (c *Controller) draining() bool {
	select {
	case <-c.drain:
		return true
	default:
		return false
	}
}

// Run scans before waiting, then after every coalesced hint or periodic tick.
// Operation store failures stop the loop; scheduling failures are reported and
// retried, and adapter failures are durably scheduled for retry.
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
	if err != nil && ctx.Err() == nil {
		c.mu.Lock()
		if c.failure == nil {
			c.failure = err
		}
		c.mu.Unlock()
	}
	// Fence admission before joining, including passes racing Run's exit.
	c.Drain()
	if err != nil {
		cancel(err)
	}
	defer cancel(nil)
	if failure := c.Wait(); failure != nil {
		return failure
	}
	return err
}

func (c *Controller) loop(ctx context.Context, ticks <-chan time.Time) error {
	for {
		if c.draining() {
			return nil
		}
		if err := c.Pass(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var schedule *scheduleError
			if !errors.As(err, &schedule) {
				return err
			}
			ghlog.Logger(ctx).Error("reconciliation schedule failed; will retry", "project", string(c.repository.Project()), "error", err.Error())
		}
		// A drain wakes a controller even when no workflow event or tick arrives.
		waitCtx, stop := context.WithCancel(ctx)
		finished := make(chan struct{})
		go func() {
			select {
			case <-c.drain:
				stop()
			case <-finished:
			}
		}()
		err := c.repository.WaitWorkflow(waitCtx, ticks)
		close(finished)
		stop()
		if c.draining() && ctx.Err() == nil {
			return nil
		}
		if err != nil {
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

// start reserves admission and durably claims in pass order. It never waits
// for workflow ownership or limit capacity. Only short bookkeeping precedes
// launching the reconciler; Inspect and Apply both run outside all trace locks.
func (c *Controller) start(ctx context.Context, stream config.WorkstreamID, record trace.OperationRecord, beside bool) (bool, error) {
	key := string(stream) + "/" + record.EventID
	c.mu.Lock()
	if c.draining() || c.running[key] {
		c.mu.Unlock()
		return false, nil
	}
	c.running[key] = true
	c.group.Add(1)
	c.mu.Unlock()
	var release func()
	admittedAttempt := false
	finish := func() {
		if release != nil {
			release()
		}
		c.mu.Lock()
		delete(c.running, key)
		c.mu.Unlock()
		if admittedAttempt {
			c.repository.WakeWorkflow()
		}
		c.group.Done()
	}
	if c.options.Limit != nil {
		var admitted bool
		release, admitted = c.options.Limit(record.Operation)
		if !admitted {
			release = nil
			finish()
			return false, nil
		}
	}
	class := c.class(record.Operation, beside)
	ctx = trace.WithSlowThreshold(ctx, c.options.SlowThreshold)
	attempt, current, err := c.repository.BeginOperation(ctx, stream, record.EventID, class, trace.Actor{Kind: "service", ID: c.options.Worker}, c.options.Now)
	if err != nil || attempt == nil {
		finish()
		return false, err
	}
	admittedAttempt = true
	run := func() error {
		defer attempt.Close()
		return c.reconcile(ctx, stream, attempt, current)
	}
	if !beside {
		defer finish()
		return true, run()
	}
	go func() {
		defer finish()
		err := run()
		c.mu.Lock()
		defer c.mu.Unlock()
		// Cancellation leaves the durable claim for restart reconciliation.
		if err != nil && ctx.Err() == nil && c.failure == nil {
			c.failure = err
			if c.abort != nil {
				c.abort(err)
			}
		}
	}()
	return true, c.failed()
}

func (c *Controller) class(op coreadapter.Operation, beside bool) trace.OperationClass {
	if c.options.Class != nil {
		return c.options.Class(op)
	}
	if beside {
		return trace.WorkerOperation
	}
	return trace.WorkflowOperation
}

// retryDelay is the wait before the next attempt of an operation that
// failed failures times before this failure.
func (c *Controller) retryDelay(failures int) time.Duration {
	delay := c.options.RetryDelay
	for i := 0; i < failures && delay < c.options.MaxRetryDelay; i++ {
		delay *= 2
	}
	return min(delay, c.options.MaxRetryDelay)
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

type scheduleError struct{ error }

func (e *scheduleError) Unwrap() error { return e.error }

// Pass discovers work from manifests and durable intent; no wakeup payload is used.
func (c *Controller) Pass(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.failed(); err != nil {
		return err
	}
	if c.draining() {
		return nil
	}
	if c.options.Schedule != nil {
		if err := c.options.Schedule(ctx); err != nil {
			return &scheduleError{err}
		}
	}
	type pending struct {
		stream config.WorkstreamID
		record trace.OperationRecord
	}
	var due []pending

	// Trace removal cannot invalidate the operation snapshot midway. The
	// snapshot guard excludes neither operation claims nor result writes.
	err := c.repository.WithWorkstreamSnapshot(func() error {
		workstreams := c.options.Workstreams
		if workstreams == nil {
			workstreams = c.repository.Workstreams
		}
		streams, err := workstreams()
		if err != nil {
			return err
		}
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
		return nil
	})
	if err != nil {
		return err
	}

	if c.options.Priority != nil {
		slices.SortStableFunc(due, func(a, b pending) int {
			return c.options.Priority(a.record.Operation) - c.options.Priority(b.record.Operation)
		})
	}
	blocked := map[config.WorkstreamID]bool{}
	for _, p := range due {
		if c.draining() {
			return nil
		}
		if err := c.step(ctx, "before-claim"); err != nil {
			return err
		}
		beside := c.options.Concurrent != nil && c.options.Concurrent(p.record.Operation)
		workflow := c.class(p.record.Operation, beside) == trace.WorkflowOperation
		if workflow && blocked[p.stream] {
			continue
		}
		admitted, err := c.start(ctx, p.stream, p.record, beside)
		if err != nil {
			return err
		}
		if workflow && !admitted {
			blocked[p.stream] = true
		}
	}
	return nil
}

// reconcile inspects and completes one admitted operation under its class's
// ordering, without holding a trace lock across an adapter call.
func (c *Controller) reconcile(ctx context.Context, stream config.WorkstreamID, attempt *trace.OperationAttempt, record trace.OperationRecord) error {
	started := time.Now()
	defer func() {
		if duration := time.Since(started); duration >= c.options.SlowThreshold {
			ghlog.Logger(ctx).Warn("operation exceeded threshold", "project", string(c.repository.Project()), "workstream", string(stream), "operation", record.Operation.ID, "action", record.Operation.Action, "duration", duration.String())
		}
	}()
	if err := c.step(ctx, "after-claim"); err != nil {
		return err
	}
	write := func(kind string) error { return attempt.Record(ctx, attempt.Action(kind, c.options.Now())) }
	retry := func(reason string) error {
		action := attempt.Action(RetryAction, c.options.Now())
		action.Failure, action.RetryAt = reason, action.At.Add(c.retryDelay(ConsecutiveFailures(record)))
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
			// Agent turns are logged separately, with the role and profile
			// only the service's turn adapter knows; every other operation
			// is logged here, the one place they all pass through.
			logger, routine := ghlog.Logger(ctx), record.Operation.Action == turnAction
			logAttrs := []any{"project", string(c.repository.Project()), "workstream", string(stream), "action", record.Operation.Action, "boundary", string(record.Operation.Boundary)}
			if !routine {
				logger.Info("operation started", logAttrs...)
			}
			start := c.options.Now()
			result, err = adapter.Apply(ctx, record.Operation)
			if e := c.step(ctx, "after-effect"); e != nil {
				return e
			}
			if !routine {
				logAttrs = append(logAttrs, "duration", c.options.Now().Sub(start).String())
				if err != nil {
					logger.Error("operation failed", append(logAttrs, "error", err.Error())...)
				} else {
					outcome := result.Outcome
					if outcome == "" {
						outcome = "ok"
					}
					logger.Info("operation finished", append(logAttrs, "outcome", outcome)...)
				}
			}
			if err != nil {
				reason := fmt.Sprintf("Effect returned error: %v", err)
				if result.Evidence != "" {
					reason += "; evidence: " + result.Evidence
				}
				return retry(reason)
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
