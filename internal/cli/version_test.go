package cli

import (
	"context"
	"path/filepath"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/buildinfo"
	"github.com/kpenfound/osmia/internal/service"
)

func stamp(t *testing.T, version, commit string) {
	t.Helper()
	oldVersion, oldCommit := buildinfo.Version, buildinfo.Commit
	buildinfo.Version, buildinfo.Commit = version, commit
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit = oldVersion, oldCommit })
}

func TestUnstampedBuildReportsDev(t *testing.T) {
	root := t.TempDir()
	if out := successful(t, root, "--version"); out != "osmia dev\n" {
		t.Fatalf("unstamped: %q", out)
	}
	stamp(t, "", "")
	if out := successful(t, root, "--version"); out != "osmia dev\n" {
		t.Fatalf("empty version: %q", out)
	}
}

func TestStampedBuildReportsVersionAndCommit(t *testing.T) {
	root := t.TempDir()
	stamp(t, "v1.2.3", "0123abc")
	if out := successful(t, root, "--version"); out != "osmia v1.2.3 (0123abc)\n" {
		t.Fatalf("stamped: %q", out)
	}
	if out := successful(t, root, "status", "--version"); out != "osmia v1.2.3 (0123abc)\n" {
		t.Fatalf("with a command: %q", out)
	}
}

func TestVersionFlagRefusesAValueAndRepetition(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"--version=true"}, {"--version", "--version"}} {
		if code, out, diag := invoke(t, root, args...); code != 2 || out != "" || diag == "" {
			t.Fatalf("%v: %d %q %q", args, code, out, diag)
		}
	}
}

func TestServeReportsStampedBuild(t *testing.T) {
	for _, c := range []struct {
		build wire.Identity
		line  string
	}{
		{wire.Identity{Version: "v1.2.3", Commit: "0123abc"}, "osmia v1.2.3 (0123abc)\n"},
		{wire.Identity{Version: "dev"}, "osmia dev\n"},
	} {
		t.Run(c.build.Version, func(t *testing.T) {
			stamp(t, c.build.Version, c.build.Commit)
			opts, _ := emptyFixture(t)
			root := opts.Config.Root
			serve(t, root)
			client := service.NewClient(filepath.Join(root, "osmia.sock"))
			defer client.Close()
			health, err := client.Health(context.Background())
			must(t, err)
			if health.Build != c.build {
				t.Fatalf("health build = %+v, want %+v", health.Build, c.build)
			}
			if out := successful(t, root, "--version"); out != c.line {
				t.Fatalf("--version %q, want %q", out, c.line)
			}
		})
	}
}
