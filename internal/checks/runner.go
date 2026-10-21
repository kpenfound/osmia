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
)

// ReviewChecks lists and runs the project's checks in a disposable export of
// a candidate. Implementations must not inherit delivery or provider
// credentials.
type ReviewChecks interface {
	// List returns the link of every check, with collections expanded to
	// their items.
	List(ctx context.Context, dir string) ([]string, error)
	// Check runs the checks the links select, or every check without links,
	// until ctx ends; callers bound it by the project's checks_timeout.
	Check(ctx context.Context, dir string, links []string) (CheckResult, error)
}

// DaggerChecks runs the project's Dagger checks through the service's engine.
// The agent receives neither the engine endpoint nor command selection.
type DaggerChecks struct{}

type CheckResult = api.CheckResult

// listOutputLimit bounds the check links read from a listing.
const listOutputLimit = 4 << 20

func (DaggerChecks) List(ctx context.Context, dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	output := &checkOutput{limit: listOutputLimit}
	exit, err := dagger(ctx, dir, output, "list", "checks", "--all", "--format=link")
	if err != nil {
		return nil, err
	}
	if exit != 0 || output.truncated {
		return nil, fmt.Errorf("dagger list checks exited %d: %s", exit, lastLines(output.String(), 5))
	}
	return checkselect.Links(output.String()), nil
}

func (DaggerChecks) Check(ctx context.Context, dir string, links []string) (CheckResult, error) {
	output := &bytes.Buffer{}
	exit, err := dagger(ctx, dir, output, append([]string{"check", "--progress=report", "--failfast"}, links...)...)
	return CheckResult{ExitCode: exit, Output: output.String()}, err
}

// dagger runs the Dagger CLI in dir with args and returns its exit status,
// -1 when it did not exit. A non-zero exit is not an error.
func dagger(ctx context.Context, dir string, output io.Writer, args ...string) (int, error) {
	home, err := os.MkdirTemp("", "osmia-check-home-")
	if err != nil {
		return -1, err
	}
	defer os.RemoveAll(home)
	// Dagger's report colors its output even off a terminal; agents read it
	// and FailedChecks parses it as plain text.
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "NO_COLOR=1"}
	// Dagger finds the workspace, and with it dagger.toml, at the enclosing
	// Git root. The export carries no repository, so it becomes its own root.
	initialise := exec.CommandContext(ctx, "git", "init", "--quiet", dir)
	initialise.Env = env
	if out, err := initialise.CombinedOutput(); err != nil {
		return -1, fmt.Errorf("git init: %w: %s", err, out)
	}
	cmd := exec.CommandContext(ctx, "dagger", args...)
	cmd.Dir = dir
	cmd.Env = slices.Clone(env)
	// Engine selection belongs to the service. No project or provider secrets
	// and no SSH agent are inherited by the check client.
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
	cmd.Stdout, cmd.Stderr = output, output
	err = cmd.Run()
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
