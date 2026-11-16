package checks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/redact"
)

// CloudRerunner asks Dagger Cloud to run the failed checks of a pull request
// again.
type CloudRerunner interface {
	// CanRerun reports why a rerun cannot be requested, or nil.
	CanRerun() error
	// RerunFailed requests the rerun from dir, a checkout of the pull
	// request's repository. It returns once the request is made, not when
	// the checks finish.
	RerunFailed(ctx context.Context, dir string, pullRequest int) error
}

// ErrCloudRerunUnavailable is a rerun that cannot be requested because no
// Dagger Cloud token is available.
var ErrCloudRerunUnavailable = errors.New("a Dagger Cloud rerun needs a Dagger Cloud token")

// maxRerunOutput bounds the output of the rerun command kept in an error.
const maxRerunOutput = 2048

// RerunCommand is the Dagger CLI invocation that reruns the failed checks of
// pull request number, without the program name.
func RerunCommand(pullRequest int) []string {
	return []string{"cloud", "checks", "rerun", "--failed", "--pr", strconv.Itoa(pullRequest)}
}

// CanRerun reports ErrCloudRerunUnavailable when the runner has no token.
func (d DaggerChecks) CanRerun() error {
	if d.CloudToken == "" {
		return ErrCloudRerunUnavailable
	}
	return nil
}

// RerunFailed runs `dagger cloud checks rerun --failed --pr <N>` in dir. Only
// the explicit allow-list reaches the CLI: a private home, the search path and
// the Cloud token, never the engine selection, since the command talks to
// Dagger Cloud and starts no engine. dir is used as it is; it is not
// prepared or initialised.
func (d DaggerChecks) RerunFailed(ctx context.Context, dir string, pullRequest int) error {
	if err := d.CanRerun(); err != nil {
		return err
	}
	redact.Register(d.CloudToken)
	home, err := os.MkdirTemp("", "osmia-rerun-home-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	program := d.Program
	if program == "" {
		program = "dagger"
	}
	cmd := exec.CommandContext(ctx, program, RerunCommand(pullRequest)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "NO_COLOR=1", "DAGGER_CLOUD_TOKEN=" + d.CloudToken}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err = cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		text := redact.Secrets(strings.TrimSpace(output.String()), redact.KnownSecrets()...)
		return fmt.Errorf("dagger %s: %v: %s", strings.Join(RerunCommand(pullRequest), " "), err, CheckExcerpt(text, maxRerunOutput))
	}
	return nil
}
