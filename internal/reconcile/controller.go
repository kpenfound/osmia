// Package reconcile runs level-triggered controllers for durable local operations.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

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
}

type Controller struct {
	repository *trace.Repository
	options    Options
	boundary   func(string) error
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
	return &Controller{repository: repository, options: options}, nil
}

// Run scans before waiting, then after every coalesced hint or periodic tick.
// Store failures stop the loop; adapter failures are durably scheduled for retry.
func (c *Controller) Run(ctx context.Context) error {
	ticks := c.options.Ticks
	if ticks == nil {
		ticker := time.NewTicker(c.options.Interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		if err := c.Pass(ctx); err != nil {
			return err
		}
		if err := c.repository.WaitWorkflow(ctx, ticks); err != nil {
			return err
		}
	}
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
	if c.options.Schedule != nil {
		if err := c.options.Schedule(ctx); err != nil {
			return err
		}
	}
	streams, err := c.repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		records, err := c.repository.Operations(stream)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.Acknowledged || c.options.Now().Before(record.RetryAt) {
				continue
			}
			if err := c.step(ctx, "before-claim"); err != nil {
				return err
			}
			err := c.repository.WithOperation(ctx, stream, record.EventID, trace.Actor{Kind: "service", ID: c.options.Worker}, c.options.Now, func(attempt *trace.OperationAttempt, current trace.OperationRecord) error {
				return c.reconcile(ctx, attempt, current)
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Controller) reconcile(ctx context.Context, attempt *trace.OperationAttempt, record trace.OperationRecord) error {
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
			result, err = adapter.Apply(ctx, record.Operation)
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
