package checks

import (
	"context"
	"fmt"
	"io"
	"regexp"

	"github.com/kpenfound/osmia/internal/redact"
)

const (
	callOutputLimit = 4 << 20
	callErrorLimit  = 64 << 10
)

var (
	callFunctionName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	callRunValue     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// ValidFunctionName reports whether name can be the Dagger function a call
// names: it starts with a letter, so it can never read as an option.
func ValidFunctionName(name string) bool { return callFunctionName.MatchString(name) }

// CallResult is the outcome of one Dagger function call. Stdout is the
// function's return value and Stderr the engine's progress output. Truncated
// reports that Stdout outgrew its limit and lost its beginning.
type CallResult struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
	TraceURL  string
}

// FunctionCaller calls one function of a project's dev module in an export
// of the project, passing it only the string argument run.
type FunctionCaller interface {
	Call(ctx context.Context, dir, function, run string) (CallResult, error)
}

// Call runs `dagger call <function> --run=<run>` in dir, an export of the
// project, through the same engine, Cloud token and clean environment as a
// check run. It passes the function no argument but run, and the process
// receives no environment beyond what a check run receives. A function that
// exits non-zero is a result, not an error.
func (d DaggerChecks) Call(ctx context.Context, dir, function, run string) (CallResult, error) {
	if !ValidFunctionName(function) {
		return CallResult{ExitCode: -1}, fmt.Errorf("%q is not a Dagger function name", function)
	}
	if !callRunValue.MatchString(run) {
		return CallResult{ExitCode: -1}, fmt.Errorf("run value %q is not a plain token", run)
	}
	stdout := &checkOutput{limit: callOutputLimit}
	stderr := &checkOutput{limit: callErrorLimit}
	var sink io.Writer = stderr
	var scan *traceScanner
	if d.CloudToken != "" {
		scan = newTraceScanner("", traceObserverFrom(ctx))
		sink = io.MultiWriter(stderr, scan)
	}
	exit, _, err := d.daggerOutput(ctx, dir, Identity{}, false, stdout, sink, "call", function, "--run="+run)
	result := CallResult{ExitCode: exit, Stdout: redact.Secrets(stdout.String(), redact.KnownSecrets()...), Stderr: redact.Secrets(stderr.String(), redact.KnownSecrets()...), Truncated: stdout.truncated}
	if scan != nil {
		scan.Flush()
		result.TraceURL = scan.URL()
	}
	return result, err
}
