package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
)

func TestDetachedProcess(t *testing.T) {
	if os.Getenv(readinessEnv) != "3" {
		return
	}
	// TestMain installs fake execution boundaries. This process has no projects.
	root := os.Args[len(os.Args)-1]
	os.Exit(Run(context.Background(), []string{"serve", "--root", root}, strings.NewReader(""), os.Stdout, os.Stderr))
}

func TestDetachedServeReadinessDuplicateOwnerAndStop(t *testing.T) {
	root, err := os.MkdirTemp("", "od-")
	must(t, err)
	defer os.RemoveAll(root)
	must(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("version = 1\nactive_projects = []\n[profiles.default]\nagent = \"claude\"\nmodel = \"test\"\n"), 0600))
	original := detachedCommand
	detachedCommand = func(root string) (*exec.Cmd, error) {
		return exec.Command(os.Args[0], "-test.run=^TestDetachedProcess$", "--", root), nil
	}
	defer func() { detachedCommand = original }()
	c := service.NewClient(filepath.Join(root, "osmia.sock"))
	defer c.Close()
	defer c.Do(context.Background(), "POST", service.Prefix+"/stop", nil, nil)
	code, out, diag := invoke(t, root, "serve", "--detach")
	if code != 0 || diag != "" || !strings.Contains(out, "Osmia is ready") {
		t.Fatalf("start %d %s %s", code, out, diag)
	}
	health, err := c.Health(context.Background())
	must(t, err)
	if !health.Ready {
		t.Fatalf("not ready: %+v", health)
	}
	code, _, _ = invoke(t, root, "serve", "--detach")
	if code != 6 {
		t.Fatalf("duplicate service started: %d", code)
	}
	info, err := os.Stat(filepath.Join(root, "service.log"))
	must(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("log permissions %s", info.Mode())
	}
	var output, diagnostics bytes.Buffer
	code = Run(context.Background(), []string{"stop", "--root", root}, strings.NewReader(""), &output, &diagnostics)
	if code != 0 {
		t.Fatalf("stop %d %s", code, diagnostics.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err = c.Health(context.Background())
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("service did not stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A detached default root starts its child without --root so the child reads
// the same XDG config file; an explicit root is passed on.
func TestDetachedRootFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "conf"))
	root, err := config.ResolveRoot("", "")
	must(t, err)
	if flag := rootFlag(root); flag != "" {
		t.Fatalf("default root flag %q", flag)
	}
	explicit, err := config.ResolveRoot(filepath.Join(home, "explicit"), "")
	must(t, err)
	if flag := rootFlag(explicit); flag != explicit.String() {
		t.Fatalf("explicit root flag %q", flag)
	}
}

// TestServeFailureNamesARuntimeFailure shows a service that stopped after it
// started naming its failure, redacted and on one line, while a startup
// failure still points at doctor without echoing its error.
func TestServeFailureNamesARuntimeFailure(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	at := time.Date(2026, 10, 3, 19, 58, 13, 0, time.UTC)
	failure := errors.Join(
		errors.New("project p_1: workstream w_1: push https://x-access-token:hunter2@github.com/acme/widgets.git refused"),
		errors.New("token ghp_secret\x1b[31m rejected"))
	var out strings.Builder
	reportServeFailure(&out, stoppedError{failure}, at)
	want := "2026-10-03T19:58:13Z service stopped after a failure: project p_1: workstream w_1: push https://[redacted]@github.com/acme/widgets.git refused; token [redacted] [31m rejected\n" +
		"fix the cause above, then start the service again\n"
	if out.String() != want {
		t.Fatalf("runtime failure:\n got %q\nwant %q", out.String(), want)
	}
	out.Reset()
	reportServeFailure(&out, errors.New("config.toml: token = \"ghp_secret\""), at)
	if got := out.String(); strings.Contains(got, "ghp_secret") || strings.Contains(got, "config.toml") || !strings.Contains(got, "osmia doctor") {
		t.Fatalf("startup failure: %q", got)
	}
	out.Reset()
	reportServeFailure(&out, stoppedError{errors.New(strings.Repeat("é", maxFailureText))}, at)
	if line, _, _ := strings.Cut(out.String(), "\n"); !strings.HasSuffix(line, "…") || !utf8.ValidString(line) || len(line) > maxFailureText+100 {
		t.Fatalf("an unbounded failure: %d bytes", len(line))
	}
}
