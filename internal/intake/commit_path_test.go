package intake

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func committedFixture(t *testing.T) (clone, commit string) {
	t.Helper()
	clone = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid"}, args...)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	fixturekit.Must(t, os.MkdirAll(filepath.Join(clone, "core", "deep"), 0700))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("committed\n"), 0600))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "core", "llm.go"), []byte("package core\n"), 0600))
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "core", "deep", "x.go"), []byte("package deep\n\n\x00"), 0600))
	fixturekit.Must(t, os.Symlink("README.md", filepath.Join(clone, "link")))
	git("add", ".")
	git("commit", "-qm", "base")
	commit = git("rev-parse", "HEAD")
	fixturekit.Must(t, os.WriteFile(filepath.Join(clone, "README.md"), []byte("uncommitted\n"), 0600))
	return clone, commit
}

func TestReadCommitPathReadsOneCommittedEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clone, commit := committedFixture(t)

	root, err := ReadCommitPath(ctx, clone, commit, ".")
	fixturekit.Must(t, err)
	if want := []string{"README.md", "core"}; !root.Dir || !slices.Equal(root.Names, want) {
		t.Fatalf("root = %+v, want directory %v", root, want)
	}
	dir, err := ReadCommitPath(ctx, clone, commit, "core")
	fixturekit.Must(t, err)
	if want := []string{"deep", "llm.go"}; !dir.Dir || !slices.Equal(dir.Names, want) {
		t.Fatalf("core = %+v, want directory %v", dir, want)
	}
	for name, want := range map[string]string{"README.md": "committed\n", "core/deep/x.go": "package deep\n\n\x00"} {
		file, err := ReadCommitPath(ctx, clone, commit, name)
		fixturekit.Must(t, err)
		if file.Dir || string(file.Data) != want {
			t.Fatalf("%s = %+v, want committed content %q", name, file, want)
		}
	}
	for _, name := range []string{"link", "missing.go", "core/missing.go", "README.md/x"} {
		if _, err := ReadCommitPath(ctx, clone, commit, name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: err = %v, want not exist", name, err)
		}
	}
}
