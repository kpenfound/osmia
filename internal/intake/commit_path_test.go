package intake

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// committedFixture commits README.md, core/llm.go, core/deep/x.go and a
// symlink, then leaves an uncommitted edit to README.md.
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

// TestReadCommitPathReadsOneCommittedEntry covers reading committed source by
// path: files and directories at the commit, never the working tree, with
// symlinks hidden as they are from a staged commit.
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

// TestCopyCommitStagesCommittedRegularFiles covers staging a whole commit:
// every regular file with its exact committed bytes, and no symlink.
func TestCopyCommitStagesCommittedRegularFiles(t *testing.T) {
	t.Parallel()
	clone, commit := committedFixture(t)
	dst := t.TempDir()
	fixturekit.Must(t, CopyCommit(context.Background(), clone, commit, dst))
	staged := map[string]string{}
	fixturekit.Must(t, filepath.WalkDir(dst, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dst, name)
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("staged non-regular file %s", rel)
		}
		data, err := os.ReadFile(name)
		staged[filepath.ToSlash(rel)] = string(data)
		return err
	}))
	want := map[string]string{"README.md": "committed\n", "core/llm.go": "package core\n", "core/deep/x.go": "package deep\n\n\x00"}
	if !maps.Equal(staged, want) {
		t.Fatalf("staged %q, want %q", staged, want)
	}
}
