package checks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/redact"
)

const (
	// MaxTraceIDLength bounds a trace ID. Dagger Cloud trace IDs are 32
	// hexadecimal characters.
	MaxTraceIDLength = 64
	// MaxTraceCheckLength bounds a check name, in bytes.
	MaxTraceCheckLength = 256
	// MaxTraceOutput bounds the trace output returned or recorded, in bytes.
	MaxTraceOutput = 64 * 1024
	// TraceFetchTimeout is how long one fetch may run when the runner sets no
	// timeout of its own.
	TraceFetchTimeout = 2 * time.Minute

	// traceCapture bounds the output held while the command runs. Only its end
	// is kept.
	traceCapture = 1 << 20
	// traceFailureExcerpt bounds the command output quoted in a failure.
	traceFailureExcerpt = 2048
	// traceKillDelay is how long a command killed at its deadline may hold the
	// fetch while its output pipes drain.
	traceKillDelay = 5 * time.Second
)

// TraceFailureReason says why a trace could not be fetched.
type TraceFailureReason string

const (
	// TraceNoToken is a service with no Dagger Cloud token.
	TraceNoToken TraceFailureReason = "no_token"
	// TraceTokenRejected is a token Dagger Cloud refused.
	TraceTokenRejected TraceFailureReason = "token_rejected"
	// TraceUnknown is a trace that does not exist or that the token's
	// organization cannot see.
	TraceUnknown TraceFailureReason = "unknown_trace"
	// TraceTimeout is a fetch that ran out of time.
	TraceTimeout TraceFailureReason = "timeout"
	// TraceCLIUnavailable is a Dagger CLI that could not run.
	TraceCLIUnavailable TraceFailureReason = "cli_unavailable"
	// TraceFailed is a failure whose cause the CLI's exit status and output do
	// not tell. Its Detail quotes a bounded, scrubbed excerpt of the output.
	TraceFailed TraceFailureReason = "failed"
)

// TraceFailure is a fetch that did not produce output. Its text holds no
// secret and no more than a bounded excerpt of the command's output.
type TraceFailure struct {
	Reason TraceFailureReason
	// Message says the cause in words.
	Message string
}

func (f *TraceFailure) Error() string { return f.Message }

func traceFailure(reason TraceFailureReason, message string) *TraceFailure {
	return &TraceFailure{Reason: reason, Message: message}
}

// TraceOutput is the output of a fetched trace after scrubbing and bounding.
type TraceOutput struct {
	// Output is the scrubbed output, at most MaxTraceOutput bytes, its end when
	// the whole is larger.
	Output string
	// Truncated reports that Output is only the end of the command's output.
	Truncated bool
	// Size is the number of bytes the command wrote.
	Size int
}

// TraceRunner reads the output of a Dagger Cloud trace, narrowed to one check
// when check is not empty. A nil failure means the fetch succeeded. The caller
// has validated both values.
type TraceRunner interface {
	FetchTrace(ctx context.Context, traceID, check string) (TraceOutput, *TraceFailure)
}

var fetchTraceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// ValidateTraceID refuses a trace ID that is empty, longer than
// MaxTraceIDLength, does not start with a letter or digit, or holds anything
// but letters, digits, `_` and `-`.
func ValidateTraceID(id string) error {
	switch {
	case id == "":
		return errors.New("trace_id is required")
	case len(id) > MaxTraceIDLength:
		return fmt.Errorf("trace_id is longer than %d characters", MaxTraceIDLength)
	case !fetchTraceIDPattern.MatchString(id):
		return errors.New("trace_id must start with a letter or digit and hold only letters, digits, `_` and `-`")
	}
	return nil
}

// ValidateTraceCheck refuses a check name that is empty, longer than
// MaxTraceCheckLength bytes, not valid UTF-8 or holds a control character.
func ValidateTraceCheck(name string) error {
	switch {
	case name == "":
		return errors.New("check must not be empty; omit it to read the whole trace")
	case len(name) > MaxTraceCheckLength:
		return fmt.Errorf("check is longer than %d bytes", MaxTraceCheckLength)
	case !utf8.ValidString(name):
		return errors.New("check must be valid UTF-8")
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return errors.New("check must not hold control characters")
		}
	}
	return nil
}

// TraceFetchCommand is the Dagger CLI invocation that shows a Dagger Cloud trace,
// without the program name: the fixed command `cloud traces view`, the check
// name as the value of the single element --check=<name>, which no value can
// turn into another flag or argument, and the trace ID, which
// ValidateTraceID has confined to start with a letter or digit, last. A
// check of "" shows the whole trace.
func TraceFetchCommand(traceID, check string) []string {
	args := []string{"cloud", "traces", "view"}
	if check != "" {
		args = append(args, "--check="+check)
	}
	return append(args, traceID)
}

// FetchTrace runs the Dagger CLI's trace command for traceID, narrowed to
// check when it is not empty, with the runner's Dagger Cloud token, and
// returns its scrubbed, bounded output. Only an explicit allow-list reaches
// the CLI: a private home and temporary directory, the search path, NO_COLOR
// and the token; never an engine selection, since the command reads from
// Dagger Cloud and starts no engine. The command runs in that private
// directory under TraceTimeout, and never through a shell. Both values must
// be validated by the caller.
func (d DaggerChecks) FetchTrace(ctx context.Context, traceID, check string) (TraceOutput, *TraceFailure) {
	if d.CloudToken == "" {
		return TraceOutput{}, traceFailure(TraceNoToken, "no Dagger Cloud token is configured for the service")
	}
	redact.Register(d.CloudToken)
	home, err := os.MkdirTemp("", "osmia-trace-home-")
	if err != nil {
		return TraceOutput{}, traceFailure(TraceCLIUnavailable, "the Dagger CLI could not run: no private working directory could be made")
	}
	defer os.RemoveAll(home)
	program := d.Program
	if program == "" {
		program = "dagger"
	}
	timeout := d.TraceTimeout
	if timeout <= 0 {
		timeout = TraceFetchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, TraceFetchCommand(traceID, check)...)
	cmd.Dir = home
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "NO_COLOR=1", "DAGGER_CLOUD_TOKEN=" + d.CloudToken}
	cmd.WaitDelay = traceKillDelay
	output := &tailBuffer{limit: traceCapture}
	cmd.Stdout, cmd.Stderr = output, output
	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return TraceOutput{}, traceFailure(TraceTimeout, fmt.Sprintf("the fetch timed out after %s", timeout))
	}
	if ctx.Err() != nil {
		return TraceOutput{}, traceFailure(TraceFailed, "the fetch was cancelled")
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return boundedTrace(output), nil
	case !errors.As(err, &exit):
		return TraceOutput{}, traceFailure(TraceCLIUnavailable, "the Dagger CLI could not run")
	}
	text := scrubbedTail(output)
	switch classifyTraceOutput(text) {
	case TraceTokenRejected:
		return TraceOutput{}, traceFailure(TraceTokenRejected, "Dagger Cloud rejected the service's token")
	case TraceUnknown:
		return TraceOutput{}, traceFailure(TraceUnknown, "the trace is unknown or not visible to the token's organization")
	}
	message := fmt.Sprintf("the fetch failed and its cause could not be told (%s)", exit)
	if excerpt := strings.TrimSpace(tailText(text, traceFailureExcerpt)); excerpt != "" {
		message += "; the end of its output: " + excerpt
	}
	return TraceOutput{}, traceFailure(TraceFailed, message)
}

var (
	tokenRejected = regexp.MustCompile(`(?i)\b(401|403)\b|unauthori[sz]ed|forbidden|invalid (api )?(token|key|credential)|bad credentials|authentication (failed|required)|not (logged in|authenticated)|token.{0,30}(expired|revoked|invalid|rejected)`)
	traceUnknown  = regexp.MustCompile(`(?i)\b404\b|not found|no such trace|unknown trace|does not exist|could not find|cannot find`)
)

// classifyTraceOutput names the cause the scrubbed output of a failed command
// tells, or "" when it tells none. A rejected token is told before an unknown
// trace, since Dagger Cloud answers an unauthenticated request for any trace
// the same way.
func classifyTraceOutput(text string) TraceFailureReason {
	switch {
	case tokenRejected.MatchString(text):
		return TraceTokenRejected
	case traceUnknown.MatchString(text):
		return TraceUnknown
	}
	return ""
}

// tailBuffer keeps the end of what is written to it and counts all of it.
type tailBuffer struct {
	limit int
	buf   []byte
	total int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	b.buf = append(b.buf, p...)
	if len(b.buf) > 2*b.limit {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.limit:]...)
	}
	return len(p), nil
}

// overflowed reports whether the buffer dropped the start of what was written.
func (b *tailBuffer) overflowed() bool { return b.total > b.limit }

// scrubbedTail returns the captured output as valid UTF-8 with the service's
// secrets and secret-shaped values redacted. When the capture dropped the
// start of the output, the first, possibly partial, line is dropped too, so no
// secret cut in two by the capture's start survives the scrubbing.
func scrubbedTail(b *tailBuffer) string {
	data := b.buf
	if len(data) > b.limit {
		data = data[len(data)-b.limit:]
	}
	if b.overflowed() {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return redact.Scrub(strings.ToValidUTF8(string(data), "�"))
}

// boundedTrace scrubs and bounds the captured output: it keeps the end, at
// most MaxTraceOutput bytes.
func boundedTrace(b *tailBuffer) TraceOutput {
	text := scrubbedTail(b)
	out := TraceOutput{Size: b.total, Truncated: b.overflowed() || len(text) > MaxTraceOutput}
	out.Output = tailText(text, MaxTraceOutput)
	return out
}

// tailText returns the last limit bytes of text, starting at a character
// boundary.
func tailText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	start := len(text) - limit
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}
