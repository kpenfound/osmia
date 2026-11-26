// Package sandbox builds and checks the osmia:latest sbx template.
//
// Build is the recipe scripts/build-sandbox.sh follows, with every one of its
// verifications, and Check reports whether the template saved now carries a
// Claude Code that runs Fable 5.2. Both run on the host that owns the sbx
// templates, need no running service and find sbx and dagger on PATH.
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Template is the sbx template name the build saves and the check reads.
	Template = "osmia:latest"

	// CodexMinVersion is the oldest Codex release whose app-server answers
	// skills/extraRoots/set, the request that gives a Codex turn its role's
	// skills (openai/codex, the app-server protocol of rust-v0.136.0).
	CodexMinVersion = "0.136.0"

	// ClaudeMinVersion is the first Claude Code release that runs Fable 5.2.
	ClaudeMinVersion = "2.1.321"

	// BuildTimeout bounds a whole build, well under the two hours a deploy
	// allows.
	BuildTimeout = time.Hour

	// CheckTimeout bounds a readiness check. A check creates one sandbox from
	// the template, runs one command in it and removes it, which takes seconds
	// to a minute.
	CheckTimeout = 5 * time.Minute

	cleanupTimeout = 2 * time.Minute
)

//go:embed guest/*.sh
var guestFiles embed.FS

func guest(name string) string {
	data, err := guestFiles.ReadFile("guest/" + name)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// GuestRemoval is the script that runs as root in the build sandbox and
// removes Docker Engine, containerd and the Docker CLI.
func GuestRemoval() string { return guest("removal.sh") }

// GuestProbe is the script that runs in the build sandbox as root and as the
// agent user and reports whether a Docker Engine is present.
func GuestProbe() string { return guest("probe.sh") }

// ClaudeInstall is the script that installs Claude Code in the build sandbox.
func ClaudeInstall() string { return guest("claude-install.sh") }

// DaggerInstall is the script that installs the Dagger CLI in the build
// sandbox, as root, with DAGGER_VERSION, BIN_DIR and CI in its environment.
func DaggerInstall() string { return guest("dagger-install.sh") }

// Host is the machine the commands run on. Its zero value runs commands with
// this process's environment and discards progress output.
type Host struct {
	// Env is the environment of every command and the PATH the tools are
	// found on. Nil inherits this process's environment.
	Env []string
	// Stdout receives progress lines and the versions found.
	Stdout io.Writer
}

func (h Host) env() []string {
	if h.Env != nil {
		return h.Env
	}
	return os.Environ()
}

func (h Host) say(format string, args ...any) {
	if h.Stdout != nil {
		fmt.Fprintf(h.Stdout, format+"\n", args...)
	}
}

// resolve finds the executable name on the PATH of the host's environment.
func (h Host) resolve(name string) (string, error) {
	path := ""
	for _, kv := range h.env() {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s is required on PATH", name)
}

// run runs name with args, feeding stdin, and returns its standard output. A
// failure carries the last line the command wrote to standard error.
func (h Host) run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	path, err := h.resolve(name)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = h.env()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, diag bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &diag
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return out.String(), fmt.Errorf("%s: %w", describe(name, args), ctx.Err())
		}
		if last := lastLine(diag.String()); last != "" {
			return out.String(), fmt.Errorf("%s: %w: %s", describe(name, args), err, last)
		}
		return out.String(), fmt.Errorf("%s: %w", describe(name, args), err)
	}
	return out.String(), nil
}

func describe(name string, args []string) string {
	words := []string{name}
	for _, a := range args {
		if strings.ContainsAny(a, "\n") {
			break
		}
		words = append(words, a)
		if len(words) == 5 {
			break
		}
	}
	return strings.Join(words, " ")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	line := strings.Join(strings.Fields(lines[len(lines)-1]), " ")
	if len(line) > 300 {
		line = line[:300]
	}
	return line
}

func (h Host) sbx(ctx context.Context, stdin string, args ...string) (string, error) {
	return h.run(ctx, stdin, "sbx", args...)
}

func uniqueName(prefix string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

// removeTemporary removes the sandbox this command created, and only that
// one, even when ctx is already cancelled.
func (h Host) removeTemporary(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if _, err := h.sbx(ctx, "", "rm", "--force", name); err != nil {
		return fmt.Errorf("could not remove temporary sandbox %s (run: sbx rm --force %s): %w", name, name, err)
	}
	return nil
}
