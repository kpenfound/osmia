package gitdiff

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=Owner", "GIT_AUTHOR_EMAIL=owner@example.invalid",
		"GIT_COMMITTER_NAME=Owner", "GIT_COMMITTER_EMAIL=owner@example.invalid",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func realDiff(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "-b", "main")

	var guide strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&guide, "line %d\n", i)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "guide.md"), []byte(guide.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "café.go"), []byte("package internal"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "base")
	base := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))

	var modified strings.Builder
	modified.WriteString("line 1\nAdded near the top.\n")
	for i := 2; i <= 19; i++ {
		fmt.Fprintf(&modified, "line %d\n", i)
	}
	modified.WriteString("updated 20\n")
	if err := os.WriteFile(filepath.Join(dir, "docs", "guide.md"), []byte(modified.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "a b.go"), []byte("package internal\n// ++ counted as an added line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "internal", "café.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02, 0x03}, 0600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "candidate")
	candidate := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))

	return git(t, dir, "diff", "--no-ext-diff", "--binary", "--no-renames", base, candidate, "--")
}

func TestParseSplitsFilesAndHunks(t *testing.T) {
	t.Parallel()
	files := Parse(realDiff(t))
	type summary struct {
		path           string
		added, removed int
		binary         bool
		hunks          int
	}
	var got []summary
	for _, f := range files {
		got = append(got, summary{f.Path, f.Added, f.Removed, f.Binary, len(f.Hunks)})
	}
	want := []summary{
		{"docs/guide.md", 2, 1, false, 2},
		{"internal/a b.go", 2, 0, false, 1},
		{"internal/café.go", 0, 1, false, 1},
		{"logo.png", 0, 0, true, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("files %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("file %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if h := files[0].Hunks[0]; h.Start != 1 || h.End != 5 {
		t.Fatalf("first hunk covers %d-%d, want 1-5", h.Start, h.End)
	}
	if h := files[0].Hunks[1]; h.Start != 18 || h.End != 21 {
		t.Fatalf("second hunk covers %d-%d, want 18-21", h.Start, h.End)
	}
	if h := files[2].Hunks[0]; h.Start != 1 || h.End != 1 {
		t.Fatalf("deletion hunk covers %d-%d, want 1-1", h.Start, h.End)
	}
}

func TestChangedFilesListsLineCounts(t *testing.T) {
	t.Parallel()
	want := "- docs/guide.md (+2 -1)\n- internal/a b.go (+2 -0)\n- internal/café.go (+0 -1)\n- logo.png (+0 -0)\n"
	if got := ChangedFiles(realDiff(t)); got != want {
		t.Fatalf("changed files %q, want %q", got, want)
	}
	if got := ChangedFiles(""); got != "(no changed files)\n" {
		t.Fatalf("empty diff lists %q", got)
	}
}
