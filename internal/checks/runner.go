package checks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/redact"
)

// ReviewChecks lists and runs the project's checks in a disposable export of
// a candidate. Implementations must not inherit delivery or provider
// credentials.
type ReviewChecks interface {
	// List returns the link of every check, with collections expanded to
	// their items.
	List(ctx context.Context, dir string, identity Identity) ([]string, error)
	// Check runs the checks the links select, or every check without links,
	// until ctx ends; callers bound it by the project's checks_timeout.
	Check(ctx context.Context, dir string, identity Identity, links []string) (CheckResult, error)
}

// DaggerChecks runs the project's Dagger checks through the service's engine.
// The agent receives neither the engine endpoint nor command selection.
// Its zero value preserves anonymous checks and host engine selection.
type DaggerChecks struct {
	// Engine selects "auto" (also the empty value), "cloud", or a resolved
	// runner address. An address overrides host engine selection.
	Engine string
	// CloudToken authenticates checks, requesting candidate CI identity too.
	// Listings receive it only when Engine is "cloud".
	CloudToken string `json:"-"`
	// CIIdentityExport requests real candidate metadata in check exports.
	CIIdentityExport bool
}

// Identity names the candidate checked by a unit, final review or session.
type Identity = api.CheckIdentity

// Preparation describes whether the check export carries the candidate identity.
// FallbackReason is empty when identity was not requested or was prepared.
type Preparation struct {
	CIIdentity     bool
	FallbackReason string
}

type CheckResult = api.CheckResult

// listOutputLimit bounds the check links read from a listing.
const listOutputLimit = 4 << 20

func (d DaggerChecks) List(ctx context.Context, dir string, identity Identity) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	output := &checkOutput{limit: listOutputLimit}
	exit, _, err := d.dagger(ctx, dir, identity, false, output, "list", "checks", "--all", "--format=link")
	if err != nil {
		return nil, err
	}
	if exit != 0 || output.truncated {
		return nil, fmt.Errorf("dagger list checks exited %d: %s", exit, lastLines(redact.Secrets(output.String(), redact.KnownSecrets()...), 5))
	}
	return checkselect.Links(redact.Secrets(output.String(), redact.KnownSecrets()...)), nil
}

// Command is the record of the check runs for links: each dagger check in the
// order it runs, joined by && because a failed run stops the rest. Empty links
// select every check.
func Command(links []string) []string {
	var command []string
	for i, addresses := range checkselect.Invocations(links) {
		if i > 0 {
			command = append(command, "&&")
		}
		command = append(append(command, "dagger"), checkArgs(addresses)...)
	}
	return command
}

func checkArgs(addresses []string) []string {
	return append([]string{"check", "--progress=report", "--fail-fast", "--parallel", "4"}, addresses...)
}

// Check runs links as the separate dagger check runs they need, in one export
// and one after another so no more than four checks run at once. The first run
// that does not pass stops the rest and supplies the exit code and any refusal.
// Output holds every run's report, and TraceURL is the trace of the latest run
// that has one.
func (d DaggerChecks) Check(ctx context.Context, dir string, identity Identity, links []string) (CheckResult, error) {
	env, preparation, cleanup, err := d.prepare(ctx, dir, identity, d.CIIdentityExport || d.CloudToken != "")
	if err != nil {
		return CheckResult{ExitCode: -1, CIIdentityFallback: preparation.FallbackReason}, redactError(err)
	}
	defer cleanup()
	result := CheckResult{CIIdentityFallback: preparation.FallbackReason}
	invocations := checkselect.Invocations(links)
	var all strings.Builder
	for _, addresses := range invocations {
		args := checkArgs(addresses)
		output := &bytes.Buffer{}
		exit, err := d.run(ctx, dir, env, preparation, output, args...)
		clean := redact.Secrets(output.String(), redact.KnownSecrets()...)
		if len(invocations) > 1 {
			fmt.Fprintf(&all, "$ dagger %s\n", strings.Join(args, " "))
		}
		all.WriteString(clean)
		if len(invocations) > 1 && !strings.HasSuffix(clean, "\n") {
			all.WriteString("\n")
		}
		result.ExitCode, result.Output, result.Refusal = exit, all.String(), refusal(clean, exit)
		if url := cloudTraceURL(clean); url != "" {
			result.TraceURL = url
		}
		if err != nil || exit != 0 {
			return result, redactError(err)
		}
	}
	return result, nil
}

// dagger runs the Dagger CLI once in dir with args and returns its exit status,
// -1 when it did not exit. A non-zero exit is not an error.
func (d DaggerChecks) dagger(ctx context.Context, dir string, identity Identity, withIdentity bool, output io.Writer, args ...string) (int, Preparation, error) {
	env, preparation, cleanup, err := d.prepare(ctx, dir, identity, withIdentity)
	if err != nil {
		return -1, preparation, redactError(err)
	}
	defer cleanup()
	exit, err := d.run(ctx, dir, env, preparation, output, args...)
	return exit, preparation, redactError(err)
}

// redactError withholds known secrets from err's message, keeping cancellation
// and other typed errors discoverable.
func redactError(err error) error {
	if err == nil {
		return nil
	}
	clean := redact.Secrets(err.Error(), redact.KnownSecrets()...)
	if clean != err.Error() {
		return &redactedError{message: clean, cause: err}
	}
	return err
}

// prepare readies dir for Dagger runs and returns the environment they share,
// and a cleanup to call once they have finished.
func (d DaggerChecks) prepare(ctx context.Context, dir string, identity Identity, withIdentity bool) (env []string, preparation Preparation, cleanup func(), err error) {
	redact.Register(d.CloudToken)
	if d.Engine == "cloud" && d.CloudToken == "" {
		return nil, Preparation{}, nil, errors.New("cloud engine requires a Dagger Cloud token")
	}
	home, err := os.MkdirTemp("", "osmia-check-home-")
	if err != nil {
		return nil, Preparation{}, nil, err
	}
	cleanup = func() { os.RemoveAll(home) }
	// Dagger's report colors its output even off a terminal; agents read it
	// and FailedChecks parses it as plain text.
	env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "NO_COLOR=1"}
	if withIdentity {
		if err := prepareIdentity(ctx, dir, env, identity); err != nil {
			preparation.FallbackReason = redact.Secrets(err.Error(), redact.KnownSecrets()...)
			if removeErr := os.RemoveAll(filepath.Join(dir, ".git")); removeErr != nil {
				cleanup()
				return nil, preparation, nil, fmt.Errorf("remove export metadata for anonymous checks: %w", removeErr)
			}
			ghlog.Logger(ctx).WarnContext(ctx, "check export uses anonymous Git identity", "candidate", redact.Secrets(identity.Candidate, redact.KnownSecrets()...), "reason", preparation.FallbackReason)
		} else {
			preparation.CIIdentity = true
		}
	}
	// Dagger finds dagger.toml at the enclosing Git root. Anonymous exports
	// need their own root; identity exports already have one.
	if !preparation.CIIdentity {
		initialise := exec.CommandContext(ctx, "git", "init", "--quiet", dir)
		initialise.Env = env
		if out, err := initialise.CombinedOutput(); err != nil {
			cleanup()
			return nil, preparation, nil, fmt.Errorf("git init: %w: %s", err, out)
		}
	}
	return env, preparation, cleanup, nil
}

// run executes the Dagger CLI in a prepared dir and returns its exit status,
// -1 when it did not exit. A non-zero exit is not an error.
func (d DaggerChecks) run(ctx context.Context, dir string, env []string, preparation Preparation, output io.Writer, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, "dagger", args...)
	cmd.Dir = dir
	cmd.Env = slices.Clone(env)
	// Only the explicit allow-list reaches the CLI. Git preparation above
	// never receives the Cloud token or engine settings.
	if d.Engine == "" || d.Engine == "auto" {
		for _, name := range []string{"DOCKER_HOST", "_EXPERIMENTAL_DAGGER_RUNNER_HOST", "DAGGER_X_RELEASE"} {
			if value := os.Getenv(name); value != "" {
				cmd.Env = append(cmd.Env, name+"="+value)
			}
		}
		if os.Getenv("DOCKER_HOST") == "" {
			if hostHome, err := os.UserHomeDir(); err == nil {
				socket := filepath.Join(hostHome, ".docker", "run", "docker.sock")
				if info, err := os.Stat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
					cmd.Env = append(cmd.Env, "DOCKER_HOST=unix://"+socket)
				}
			}
		}
	} else {
		// DAGGER_ENGINE takes precedence over the deprecated runner and Cloud
		// variables; none of the host's engine selectors are inherited here.
		cmd.Env = append(cmd.Env, "DAGGER_ENGINE="+d.Engine)
		if d.Engine != "cloud" {
			cmd.Env = append(cmd.Env, "_EXPERIMENTAL_DAGGER_RUNNER_HOST="+d.Engine)
		}
		if release := os.Getenv("DAGGER_X_RELEASE"); release != "" {
			cmd.Env = append(cmd.Env, "DAGGER_X_RELEASE="+release)
		}
	}
	isCheck := len(args) > 0 && args[0] == "check"
	if d.CloudToken != "" && (isCheck || d.Engine == "cloud") {
		cmd.Env = append(cmd.Env, "DAGGER_CLOUD_TOKEN="+d.CloudToken)
	}
	if isCheck && d.CloudToken != "" && preparation.CIIdentity {
		cmd.Env = append(cmd.Env, "CI=true")
	}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return exit, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exit, nil
	}
	return exit, err
}

// checkOutput keeps the last limit bytes written to it, starting at a whole
// UTF-8 character, and whether anything before them was dropped.
type checkOutput struct {
	mu        sync.Mutex
	limit     int
	text      []byte
	truncated bool
}

func (b *checkOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text = append(b.text, p...)
	if over := len(b.text) - b.limit; over > 0 {
		for over < len(b.text) && !utf8.RuneStart(b.text[over]) {
			over++
		}
		b.text = append(b.text[:0], b.text[over:]...)
		b.truncated = true
	}
	return len(p), nil
}
func (b *checkOutput) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.text) }

// lastLines returns the last n non-empty lines of text.
func lastLines(text string, n int) string {
	lines := slices.DeleteFunc(strings.Split(strings.TrimSpace(text), "\n"), func(l string) bool { return strings.TrimSpace(l) == "" })
	return strings.Join(lines[max(len(lines)-n, 0):], "\n")
}

// redactedError keeps cancellation and other typed errors discoverable.
type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }
