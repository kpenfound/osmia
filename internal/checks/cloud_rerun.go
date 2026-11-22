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
	// request's repository, in the Dagger Cloud organization org, or in the
	// one Dagger Cloud infers when org is empty. It returns once the request
	// is made, not when the checks finish.
	RerunFailed(ctx context.Context, dir string, pullRequest int, org string) error
	// RerunAll requests the rerun of every check of the pull request, from
	// dir and in org, with the same return as RerunFailed.
	RerunAll(ctx context.Context, dir string, pullRequest int, org string) error
}

// ErrCloudRerunUnavailable is a rerun that cannot be requested because no
// Dagger Cloud token is available.
var ErrCloudRerunUnavailable = errors.New("a Dagger Cloud rerun needs a Dagger Cloud API token")

// maxRerunOutput bounds the output of the rerun command kept in an error.
const maxRerunOutput = 2048

// RerunCommand is the Dagger CLI invocation that reruns the failed checks of
// pull request number, without the program name. A non-empty org is passed
// as --org.
func RerunCommand(pullRequest int, org string) []string {
	return withOrganization([]string{"cloud", "checks", "rerun", "--failed", "--pr", strconv.Itoa(pullRequest)}, org)
}

// RerunAllCommand is the Dagger CLI invocation that reruns every check of
// pull request number, without the program name. A non-empty org is passed
// as --org.
func RerunAllCommand(pullRequest int, org string) []string {
	return withOrganization([]string{"cloud", "checks", "rerun", "--all", "--pr", strconv.Itoa(pullRequest)}, org)
}

func withOrganization(args []string, org string) []string {
	if org == "" {
		return args
	}
	return append(args, "--org", org)
}

// CanRerun reports ErrCloudRerunUnavailable when the runner has no token.
func (d DaggerChecks) CanRerun() error {
	if d.CloudToken == "" {
		return ErrCloudRerunUnavailable
	}
	return nil
}

// RerunFailed runs `dagger cloud checks rerun --failed --pr <N> [--org <org>]` in dir. Only
// the explicit allow-list reaches the CLI: a private home, the search path and
// the Cloud token, never the engine selection, since the command talks to
// Dagger Cloud and starts no engine. dir is used as it is; it is not
// prepared or initialised.
func (d DaggerChecks) RerunFailed(ctx context.Context, dir string, pullRequest int, org string) error {
	return d.rerun(ctx, dir, RerunCommand(pullRequest, org))
}

// RerunAll runs `dagger cloud checks rerun --all --pr <N> [--org <org>]` in
// dir, under the same rules as RerunFailed.
func (d DaggerChecks) RerunAll(ctx context.Context, dir string, pullRequest int, org string) error {
	return d.rerun(ctx, dir, RerunAllCommand(pullRequest, org))
}

func (d DaggerChecks) rerun(ctx context.Context, dir string, args []string) error {
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
	cmd := exec.CommandContext(ctx, program, args...)
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
		return fmt.Errorf("dagger %s: %v: %s", strings.Join(args, " "), err, CheckExcerpt(text, maxRerunOutput))
	}
	return nil
}
