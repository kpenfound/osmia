package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/buildinfo"
	"github.com/kpenfound/osmia/internal/service"
)

// stamp sets the link-time build identity for the rest of the test.
func stamp(t *testing.T, version, commit string) {
	t.Helper()
	oldVersion, oldCommit := buildinfo.Version, buildinfo.Commit
	buildinfo.Version, buildinfo.Commit = version, commit
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit = oldVersion, oldCommit })
}

// TestVersionWithoutService shows --version answering with no service
// running: the default identity of an unstamped build, and a stamped one.
func TestVersionWithoutService(t *testing.T) {
	root := t.TempDir()
	if out := successful(t, root, "--version"); out != "osmia dev\n" {
		t.Fatalf("unstamped: %q", out)
	}
	stamp(t, "", "")
	if out := successful(t, root, "--version"); out != "osmia dev\n" {
		t.Fatalf("empty version: %q", out)
	}
	stamp(t, "v1.2.3", "0123abc")
	if out := successful(t, root, "--version"); out != "osmia v1.2.3 (0123abc)\n" {
		t.Fatalf("stamped: %q", out)
	}
	if out := successful(t, root, "status", "--version"); out != "osmia v1.2.3 (0123abc)\n" {
		t.Fatalf("with a command: %q", out)
	}
	for _, args := range [][]string{{"--version=true"}, {"--version", "--version"}} {
		if code, out, diag := invoke(t, root, args...); code != 2 || out != "" || diag == "" {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}
}

// TestServeReportsStampedBuild shows osmia serve reporting the same stamped
// identity in /v1/health that --version prints.
func TestServeReportsStampedBuild(t *testing.T) {
	for _, b := range []service.Identity{{Version: "v1.2.3", Commit: "0123abc"}, {Version: "dev"}} {
		t.Run(b.Version, func(t *testing.T) {
			stamp(t, b.Version, b.Commit)
			opts, _ := emptyFixture(t)
			root := opts.Config.Root
			serve(t, root)
			c := service.NewClient(filepath.Join(root, "osmia.sock"))
			defer c.Close()
			health, err := c.Health(context.Background())
			must(t, err)
			if health.Build != b {
				t.Fatalf("health build = %+v, want %+v", health.Build, b)
			}
			if out := successful(t, root, "--version"); out != versionLine(health.Build)+"\n" {
				t.Fatalf("--version %q disagrees with health %+v", out, health.Build)
			}
		})
	}
}
