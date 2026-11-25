package climb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/checks"
)

// RunStatus is how one evaluator run ended.
type RunStatus string

const (
	// RunScored is a run whose output conforms to the contract, reports
	// valid true and carries every required metric as the climb spec and
	// calibration expect it.
	RunScored RunStatus = "scored"
	// RunInvalid is a run whose evaluator reported valid false. It scores
	// nothing.
	RunInvalid RunStatus = "invalid"
	// RunFailed is a run that did not produce a usable output: the function
	// failed or timed out, the output does not conform, or a required metric
	// is missing or differs from what is expected.
	RunFailed RunStatus = "failed"
)

// Run is the record of one evaluator invocation: the random value it was
// called with, the full output and the values parsed from it.
type Run struct {
	Number     int       `json:"number"`
	Value      string    `json:"run"`
	Status     RunStatus `json:"status"`
	Reason     string    `json:"reason,omitempty"`
	Output     string    `json:"output"`
	Stderr     string    `json:"stderr,omitempty"`
	Parsed     *Output   `json:"parsed,omitempty"`
	TraceURL   string    `json:"trace_url,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
}

// Source exports a commit of the project into an empty directory.
// workspace.Provider implementations satisfy it.
type Source interface {
	Export(ctx context.Context, commit, dir string) error
}

// Runner measures a candidate by calling the evaluator function of the
// project's dev module through Dagger. Each run happens in a fresh export of
// the commit with the approved copy of the protected paths restored, holds a
// capacity.checks slot while the function runs, and ends at its own timeout.
// The engine is the check client, so a run has the check client's environment
// and no other secrets.
type Runner struct {
	Source Source
	Engine checks.FunctionCaller
	// AcquireSlot takes a capacity.checks slot and returns its release.
	AcquireSlot func(ctx context.Context) (release func(), err error)
	// Root is the directory that holds the fresh exports.
	Root string
	// Timeout bounds one evaluator call.
	Timeout time.Duration
	// Now stamps runs; nil means time.Now.
	Now func() time.Time
	// RunValue returns the value passed as the evaluator's run argument; nil
	// means a fresh 128-bit random token.
	RunValue func() (string, error)
}

// Request describes what to measure.
type Request struct {
	// Commit is the candidate to export.
	Commit string
	// Function is the evaluator's name in the dev module.
	Function string
	// Approved restores the frozen protected paths into each export.
	Approved Restorer
	// Expect lists the metrics every run must report.
	Expect Expectations
	// Repeats is how many runs the measurement takes, at least 1.
	Repeats int
	// Progress, when set, is called before each run with the run's number.
	Progress func(run int)
}

func (q Request) validate() error {
	switch {
	case q.Commit == "":
		return errors.New("no commit to measure")
	case !checks.ValidFunctionName(q.Function):
		return fmt.Errorf("%q is not an evaluator function name", q.Function)
	case q.Approved == nil:
		return errors.New("no approved copy of the protected paths to restore")
	case q.Repeats < 1:
		return fmt.Errorf("a measurement needs at least one run, not %d", q.Repeats)
	}
	return q.Expect.Validate()
}

func (r Runner) validate() error {
	switch {
	case r.Source == nil || r.Engine == nil || r.AcquireSlot == nil:
		return errors.New("the measurement runner is not fully configured")
	case r.Root == "":
		return errors.New("the measurement runner has no export directory")
	case r.Timeout <= 0:
		return errors.New("the measurement timeout is not set")
	}
	return nil
}

// Measure runs the evaluator req.Repeats times, one run after another, and
// aggregates them. It stops at the first run that does not score, because the
// measurement is then invalid whatever the others would report. It returns an
// error only when a run could not be carried out at all, such as a failed
// export or a cancelled context; a failed or invalid evaluator run is data
// in the Measurement.
func (r Runner) Measure(ctx context.Context, req Request) (Measurement, error) {
	if err := r.validate(); err != nil {
		return Measurement{}, err
	}
	if err := req.validate(); err != nil {
		return Measurement{}, err
	}
	var runs []Run
	for number := 1; number <= req.Repeats; number++ {
		if req.Progress != nil {
			req.Progress(number)
		}
		run, err := r.once(ctx, req, number)
		if err != nil {
			return Measurement{}, err
		}
		runs = append(runs, run)
		if run.Status != RunScored {
			break
		}
	}
	return Aggregate(req.Expect, req.Repeats, runs), nil
}

// Calibrate measures n runs of the evaluator on the evaluator commit and
// derives the calibration verdict for a minimum improvement.
func (r Runner) Calibrate(ctx context.Context, req Request, n int, minImprovement float64) (Calibration, error) {
	req.Repeats = n
	m, err := r.Measure(ctx, req)
	if err != nil {
		return Calibration{}, err
	}
	return Calibrate(m, minImprovement), nil
}

func (r Runner) once(ctx context.Context, req Request, number int) (Run, error) {
	value, err := r.runValue()
	if err != nil {
		return Run{}, err
	}
	if err := os.MkdirAll(r.Root, 0o700); err != nil {
		return Run{}, err
	}
	dir, err := os.MkdirTemp(r.Root, "measure-")
	if err != nil {
		return Run{}, err
	}
	defer os.RemoveAll(dir)
	if err := r.Source.Export(ctx, req.Commit, dir); err != nil {
		return Run{}, fmt.Errorf("export %s for measurement: %w", req.Commit, err)
	}
	if err := req.Approved.Restore(ctx, dir); err != nil {
		return Run{}, fmt.Errorf("restore the protected paths: %w", err)
	}
	release, err := r.AcquireSlot(ctx)
	if err != nil {
		return Run{}, err
	}
	defer release()
	start := r.now()
	callCtx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	result, err := r.Engine.Call(callCtx, dir, req.Function, value)
	if ctx.Err() != nil {
		return Run{}, ctx.Err()
	}
	run := Run{Number: number, Value: value, StartedAt: start.UTC(), Output: result.Stdout, TraceURL: result.TraceURL}
	switch {
	case errors.Is(callCtx.Err(), context.DeadlineExceeded):
		run.fail(fmt.Sprintf("the evaluator did not finish within %s", r.Timeout), result)
	case err != nil:
		return Run{}, fmt.Errorf("call the evaluator: %w", err)
	default:
		run.classify(req.Expect, result)
	}
	run.DurationMS = r.now().Sub(start).Milliseconds()
	return run, nil
}

func (run *Run) fail(reason string, result checks.CallResult) {
	run.Status, run.Reason = RunFailed, reason
	run.Stderr = tail(result.Stderr, 4000)
}

func (run *Run) classify(expect Expectations, result checks.CallResult) {
	switch {
	case result.ExitCode != 0:
		run.fail(fmt.Sprintf("the evaluator exited %d: %s", result.ExitCode, tail(strings.TrimSpace(result.Stderr), 600)), result)
		return
	case result.Truncated:
		run.fail("the evaluator output is too large to read", result)
		return
	}
	out, err := ParseOutput(result.Stdout)
	if err != nil {
		run.fail(err.Error(), result)
		return
	}
	run.Parsed = &out
	if !out.Valid {
		run.Status, run.Reason = RunInvalid, strings.TrimSpace(out.Reason)
		if run.Reason == "" {
			run.Reason = "the evaluator reported valid: false without a reason"
		}
		return
	}
	if err := expect.Check(out); err != nil {
		run.fail(err.Error(), result)
		return
	}
	run.Status = RunScored
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r Runner) runValue() (string, error) {
	if r.RunValue != nil {
		return r.RunValue()
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[len(s)-n:], "")
}
