package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
