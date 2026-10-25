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

func (d DaggerChecks) Check(ctx context.Context, dir string, identity Identity, links []string) (CheckResult, error) {
	output := &bytes.Buffer{}
	exit, preparation, err := d.dagger(ctx, dir, identity, d.CIIdentityExport || d.CloudToken != "", output, append([]string{"check", "--progress=report", "--fail-fast"}, links...)...)
	clean := redact.Secrets(output.String(), redact.KnownSecrets()...)
	return CheckResult{ExitCode: exit, Output: clean, TraceURL: cloudTraceURL(clean), CIIdentityFallback: preparation.FallbackReason}, err
}

// dagger runs the Dagger CLI in dir with args and returns its exit status,
// -1 when it did not exit. A non-zero exit is not an error.
func (d DaggerChecks) dagger(ctx context.Context, dir string, identity Identity, withIdentity bool, output io.Writer, args ...string) (exitCode int, preparation Preparation, runErr error) {
	redact.Register(d.CloudToken)
	defer func() {
		if runErr != nil {
			clean := redact.Secrets(runErr.Error(), redact.KnownSecrets()...)
			if clean != runErr.Error() {
				runErr = &redactedError{message: clean, cause: runErr}
			}
		}
	}()
	if d.Engine == "cloud" && d.CloudToken == "" {
		return -1, Preparation{}, errors.New("cloud engine requires a Dagger Cloud token")
	}
	home, err := os.MkdirTemp("", "osmia-check-home-")
	if err != nil {
		return -1, Preparation{}, err
	}
	defer os.RemoveAll(home)
	// Dagger's report colors its output even off a terminal; agents read it
	// and FailedChecks parses it as plain text.
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "NO_COLOR=1"}
	preparation = Preparation{}
	if withIdentity {
		if err := prepareIdentity(ctx, dir, env, identity); err != nil {
			preparation.FallbackReason = redact.Secrets(err.Error(), redact.KnownSecrets()...)
			if removeErr := os.RemoveAll(filepath.Join(dir, ".git")); removeErr != nil {
				return -1, preparation, fmt.Errorf("remove export metadata for anonymous checks: %w", removeErr)
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
			return -1, preparation, fmt.Errorf("git init: %w: %s", err, out)
		}
	}
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
	err = cmd.Run()
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return exit, preparation, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exit, preparation, nil
	}
	return exit, preparation, err
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
