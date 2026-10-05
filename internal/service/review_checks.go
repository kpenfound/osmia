package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
)

// CheckResult records the exit status of a candidate check and the end of
// its output, where Dagger's report is, bounded to 64 KiB.
type CheckResult struct {
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

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

// checkOutputLimit bounds a check's recorded output; listOutputLimit bounds
// the check links read from a listing.
const (
	checkOutputLimit = 64 * 1024
	listOutputLimit  = 4 << 20
)

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
	output := &checkOutput{limit: checkOutputLimit}
	exit, err := dagger(ctx, dir, output, append([]string{"check", "--progress=report"}, links...)...)
	return CheckResult{ExitCode: exit, Output: output.String(), Truncated: output.truncated}, err
}

// dagger runs the Dagger CLI in dir with args and returns its exit status,
// -1 when it did not exit. A non-zero exit is not an error.
func dagger(ctx context.Context, dir string, output io.Writer, args ...string) (int, error) {
	home, err := os.MkdirTemp("", "osmia-check-home-")
	if err != nil {
		return -1, err
	}
	defer os.RemoveAll(home)
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home}
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

// failedCheck matches a failed check in the CHECKS section of Dagger's
// report, capturing its link.
var failedCheck = regexp.MustCompile(`(?m)^✘ (dag(?:\+check)?://\S+)`)

// failedChecks returns the links of the checks Dagger's report shows failed.
func failedChecks(output string) []string {
	_, report, found := strings.Cut(output, "== CHECKS ==")
	if !found {
		return nil
	}
	var links []string
	for _, m := range failedCheck.FindAllStringSubmatch(report, -1) {
		links = append(links, m[1])
	}
	return links
}

// unitReviewerIdentity binds reads and checks to the queued turn's candidate,
// including answer turns that continue an earlier review.
func unitReviewerIdentity(r *trace.Repository, scope coreadapter.Scope) (UnitReviewIdentity, error) {
	stream := config.WorkstreamID(scope.Workstream)
	thread, err := r.Thread(stream, scope.Thread)
	if err != nil {
		return UnitReviewIdentity{}, err
	}
	for _, turn := range thread.Turns {
		if turn.Request.TurnID == scope.Turn {
			reader := &reviewers{masons: &masons{repository: r}}
			identity, err := reader.turnIdentity(stream, scope.Unit, turn)
			if err != nil {
				return identity, err
			}
			if identity.Subject != scope.Workstream+"/"+scope.Unit || identity.Candidate.Revision == "" {
				return identity, errors.New("review candidate does not match the turn")
			}
			return identity, nil
		}
	}
	return UnitReviewIdentity{}, errors.New("review turn not found")
}

func unitReviewerSelection(ctx context.Context, cfg *config.Config, r *trace.Repository, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	identity, err := unitReviewerIdentity(r, scope)
	if err != nil {
		return isolation.Selection{}, err
	}
	provider, err := newUnitWorkspaces(cfg, r).of(config.WorkstreamID(scope.Workstream))
	if err != nil {
		return isolation.Selection{}, err
	}
	// A thread runs one turn at a time, so the exports of its earlier turns
	// are done with.
	exports := filepath.Join(cfg.Root.String(), reviewInputsDirectory, scope.Project, scope.Workstream, scope.Thread)
	dir := filepath.Join(exports, scope.Turn)
	if err := os.RemoveAll(exports); err != nil {
		return isolation.Selection{}, err
	}
	if err := provider.Export(ctx, identity.Candidate.Revision, dir); err != nil {
		return isolation.Selection{}, err
	}
	paths, err := viewPaths(dir)
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: dir, Directory: dir, BaseRevision: identity.Candidate.Revision}, Paths: paths, Execution: execution}, err
}
