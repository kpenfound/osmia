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
	"github.com/kpenfound/osmia/internal/engine"
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

// SkippingChecks is a ReviewChecks that can leave checks it names out of a run.
type SkippingChecks interface {
	ReviewChecks
	// CheckSkipping is Check without the checks the skip links select, which
	// already passed. A run that has nothing left to run passes.
	CheckSkipping(ctx context.Context, dir string, identity Identity, links, skip []string) (CheckResult, error)
}

// DaggerChecks runs the project's Dagger checks through the service's engine.
// The agent receives neither the engine endpoint nor command selection.
// Its zero value preserves anonymous checks and host engine selection.
type DaggerChecks struct {
	// Engine selects "auto" (also the empty value), "cloud", or a resolved
	// runner address. An address overrides host engine selection.
	Engine string
	// CloudToken authenticates checks, which also request candidate identity
	// in their exports.
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

// ResultCommand returns the commands actually started by the runner, or the
// selection's command for runners that do not supply execution details.
func ResultCommand(result CheckResult, links []string) []string {
	if result.Command != nil {
		return result.Command
	}
	return Command(links)
}

func checkArgs(addresses []string) []string {
	return append([]string{"check", "--progress=report", "--fail-fast", "--parallel", "4"}, addresses...)
}

// skipArgs are checkArgs that leave out the checks skip selects.
func skipArgs(addresses, skip []string) []string {
	args := checkArgs(nil)
	for _, link := range skip {
		args = append(args, "--skip", link)
	}
	return append(args, addresses...)
}

// Check runs links as the separate dagger check runs they need, in one export
// and one after another so no more than four checks run at once. A run whose
// report shows failed checks reruns them once (see checkRun.view). A check that
// passes its rerun counts as passed and is listed in Flaky. The first run that
// does not pass stops the rest and supplies the exit code and any refusal.
// Output holds every run's report, and TraceURL is the trace of the latest run
// that has one.
func (d DaggerChecks) Check(ctx context.Context, dir string, identity Identity, links []string) (CheckResult, error) {
	return d.CheckSkipping(ctx, dir, identity, links, nil)
}

// CheckSkipping is Check without the checks the skip links select.
func (d DaggerChecks) CheckSkipping(ctx context.Context, dir string, identity Identity, links, skip []string) (CheckResult, error) {
	env, preparation, cleanup, err := d.prepare(ctx, dir, identity, d.CIIdentityExport || d.CloudToken != "")
	if err != nil {
		return CheckResult{ExitCode: -1, CIIdentityFallback: preparation.FallbackReason}, redactError(err)
	}
	defer cleanup()
	invocations := checkselect.Invocations(links)
	run := checkRun{checks: d, dir: dir, env: env, labelled: len(invocations) > 1}
	run.result.CIIdentityFallback = preparation.FallbackReason
	for _, addresses := range invocations {
		if err := run.view(ctx, addresses, skip); err != nil || run.result.ExitCode != 0 {
			return run.result, redactError(err)
		}
	}
	return run.result, nil
}

// checkRun accumulates the dagger check runs of one Check into its result.
type checkRun struct {
	checks      DaggerChecks
	dir         string
	env         []string
	result      CheckResult
	output      strings.Builder
	// labelled heads each report with its command, once there is more than one.
	labelled bool
}

// view runs one set of addresses to a result, giving each failed check one
// rerun by its own link.
//
// Dagger caches a failed result under the address that ran it, so the rerun is
// a fresh attempt only when its link selects less than the run that failed; the
// same address replays the failure, which then stands. That cache also means
// the failed run cannot simply be repeated once the check passes. With
// --fail-fast, that run may have cancelled checks its report does not mention,
// so a passed rerun does not yet show the addresses pass: the addresses run
// again skipping what passed, and any new failure gets its own rerun.
//
// Checks in skip are left out of every run, as if they had passed on a rerun.
func (r *checkRun) view(ctx context.Context, addresses, skip []string) error {
	flaky := slices.Clone(skip)
	for {
		args := checkArgs(addresses)
		if len(flaky) > 0 {
			args = skipArgs(addresses, flaky)
		}
		exit, report, err := r.attempt(ctx, args)
		r.result.ExitCode, r.result.Refusal = exit, refusal(report, exit)
		if len(flaky) > 0 && reportNothingSelected.MatchString(r.result.Refusal) {
			r.result.ExitCode, r.result.Refusal = 0, ""
			return nil
		}
		if err != nil || exit == 0 {
			return err
		}
		failed := FailedChecks(report)
		if len(failed) == 0 || slices.ContainsFunc(failed, func(link string) bool { return slices.Contains(flaky, link) }) {
			return nil
		}
		passed, err := r.rerun(ctx, failed)
		r.result.Flaky = append(r.result.Flaky, passed...)
		if err != nil || len(passed) < len(failed) {
			return err
		}
		flaky = append(flaky, passed...)
	}
}

// rerun runs failed once more and returns those that passed, leaving the failed
// run's exit code. A rerun that reports no result, including one that outlasts
// the run's deadline, shows nothing against the failure, which stands. Only
// cancellation returns an error.
func (r *checkRun) rerun(ctx context.Context, failed []string) ([]string, error) {
	r.labelled = true
	allPassed := true
	for _, addresses := range checkselect.Invocations(failed) {
		exit, _, err := r.attempt(ctx, checkArgs(addresses))
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, err
		}
		allPassed = allPassed && err == nil && exit == 0
	}
	if allPassed {
		return failed, nil
	}
	latest := FailedChecks(r.output.String())
	return slices.DeleteFunc(slices.Clone(failed), func(link string) bool { return slices.Contains(latest, link) }), nil
}

// attempt runs dagger check once, adding its report to the result's output, and
// returns its exit status and its report alone.
func (r *checkRun) attempt(ctx context.Context, args []string) (int, string, error) {
	if len(r.result.Command) > 0 {
		r.result.Command = append(r.result.Command, "&&")
	}
	r.result.Command = append(append(r.result.Command, "dagger"), args...)
	var output bytes.Buffer
	exit, err := r.checks.run(ctx, r.dir, r.env, &output, args...)
	report := redact.Secrets(output.String(), redact.KnownSecrets()...)
	if r.labelled {
		r.endLine()
		fmt.Fprintf(&r.output, "$ dagger %s\n", strings.Join(args, " "))
	}
	r.output.WriteString(report)
	if r.labelled {
		r.endLine()
	}
	r.result.Output = r.output.String()
	r.result.TraceURL = cloudTraceURL(report)
	return exit, report, err
}

func (r *checkRun) endLine() {
	if text := r.output.String(); text != "" && !strings.HasSuffix(text, "\n") {
		r.output.WriteString("\n")
	}
}

// dagger runs the Dagger CLI once in dir with args and returns its exit status,
// -1 when it did not exit. A non-zero exit is not an error.
func (d DaggerChecks) dagger(ctx context.Context, dir string, identity Identity, withIdentity bool, output io.Writer, args ...string) (int, Preparation, error) {
	return d.daggerOutput(ctx, dir, identity, withIdentity, output, output, args...)
}

func (d DaggerChecks) daggerOutput(ctx context.Context, dir string, identity Identity, withIdentity bool, stdout, stderr io.Writer, args ...string) (int, Preparation, error) {
	env, preparation, cleanup, err := d.prepare(ctx, dir, identity, withIdentity)
	if err != nil {
		return -1, preparation, redactError(err)
	}
	defer cleanup()
	exit, err := d.runOutput(ctx, dir, env, stdout, stderr, args...)
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
	if err := engine.ValidateDockerSelectors(d.Engine, os.Getenv); err != nil {
		return nil, Preparation{}, nil, err
	}
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
func (d DaggerChecks) run(ctx context.Context, dir string, env []string, output io.Writer, args ...string) (int, error) {
	return d.runOutput(ctx, dir, env, output, output, args...)
}

func (d DaggerChecks) runOutput(ctx context.Context, dir string, env []string, stdout, stderr io.Writer, args ...string) (int, error) {
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
		// variables. Docker container addresses also need the service Docker
		// host; other Docker and SSH settings remain outside this allow-list.
		cmd.Env = append(cmd.Env, "DAGGER_ENGINE="+d.Engine)
		if strings.HasPrefix(d.Engine, "docker-container://") {
			if host := os.Getenv("DOCKER_HOST"); host != "" {
				cmd.Env = append(cmd.Env, "DOCKER_HOST="+host)
			}
		}
		if d.Engine != "cloud" {
			cmd.Env = append(cmd.Env, "_EXPERIMENTAL_DAGGER_RUNNER_HOST="+d.Engine)
		}
		if release := os.Getenv("DAGGER_X_RELEASE"); release != "" {
			cmd.Env = append(cmd.Env, "DAGGER_X_RELEASE="+release)
		}
	}
	isCheck := len(args) > 0 && args[0] == "check"
	isPreview := len(args) > 0 && (args[0] == "call" || args[0] == "core")
	if d.CloudToken != "" && (isCheck || isPreview || d.Engine == "cloud") {
		cmd.Env = append(cmd.Env, "DAGGER_CLOUD_TOKEN="+d.CloudToken)
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
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
