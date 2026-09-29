package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
)

const runChecksTool = "run_checks"

// CheckResult records bounded output and the exit status of a candidate check.
type CheckResult struct {
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

// ReviewChecks runs the fixed project check in the supplied disposable export.
// Implementations must not inherit delivery or provider credentials.
type ReviewChecks interface {
	Check(context.Context, string) (CheckResult, error)
}

// DaggerChecks runs the project's Dagger checks through the service's engine.
// The agent receives neither the engine endpoint nor command selection.
type DaggerChecks struct{}

func (DaggerChecks) Check(ctx context.Context, dir string) (CheckResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	home, err := os.MkdirTemp("", "osmia-check-home-")
	if err != nil {
		return CheckResult{}, err
	}
	defer os.RemoveAll(home)
	cmd := exec.CommandContext(ctx, "dagger", "check")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home}
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
	output := &checkOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	err = cmd.Run()
	result := CheckResult{ExitCode: -1, Output: output.String(), Truncated: output.truncated}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return result, nil
	}
	return result, err
}

type checkOutput struct {
	mu        sync.Mutex
	text      strings.Builder
	truncated bool
}

func (b *checkOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := 64*1024 - b.text.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.text.Write(p)
	return n, nil
}
func (b *checkOutput) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.text.String() }

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
	dir := filepath.Join(cfg.Root.String(), "review-inputs", scope.Project, scope.Workstream, scope.Thread, scope.Turn)
	if err := os.RemoveAll(dir); err != nil {
		return isolation.Selection{}, err
	}
	if err := provider.Export(ctx, identity.Candidate.Revision, dir); err != nil {
		return isolation.Selection{}, err
	}
	paths, err := viewPaths(dir)
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: dir, Directory: dir, BaseRevision: identity.Candidate.Revision}, Paths: paths, Execution: execution}, err
}

func candidateCheckTool(cfg *config.Config, r *trace.Repository, scope coreadapter.Scope, commit string, checks ReviewChecks) coreadapter.Tool {
	var mu sync.Mutex
	return coreadapter.Tool{Name: runChecksTool, Description: "Run dagger check on a fresh disposable copy of this review's exact candidate; returns its commit, exit status and bounded output.", Effect: coreadapter.ToolCheck,
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var args map[string]json.RawMessage
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, err
			}
			if args == nil || len(args) != 0 {
				return nil, errors.New("run_checks accepts no command, path or environment overrides")
			}
			if checks == nil {
				return nil, errors.New("candidate check runner is unavailable")
			}
			mu.Lock()
			defer mu.Unlock()
			provider, err := newUnitWorkspaces(cfg, r).of(config.WorkstreamID(scope.Workstream))
			if err != nil {
				return nil, err
			}
			base := filepath.Join(cfg.Root.String(), "checks")
			if err := os.MkdirAll(base, 0700); err != nil {
				return nil, err
			}
			dir, err := os.MkdirTemp(base, "candidate-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(dir)
			if err := provider.Export(ctx, commit, dir); err != nil {
				return nil, err
			}
			result, err := checks.Check(ctx, dir)
			response := struct {
				Candidate string `json:"candidate"`
				Check     string `json:"check"`
				CheckResult
				Error string `json:"error,omitempty"`
			}{Candidate: commit, Check: "dagger check", CheckResult: result}
			if err != nil {
				response.Error = fmt.Sprintf("candidate check failed: %v", err)
			}
			return json.Marshal(response)
		},
	}
}
