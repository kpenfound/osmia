package kb

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/workspace"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR="}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitTree(t *testing.T, files, links map[string]string) (clone, commit string) {
	t.Helper()
	clone = t.TempDir()
	git(t, clone, "init", "--quiet")
	writeTree(t, clone, files)
	for name, target := range links {
		full := filepath.Join(clone, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
	}
	git(t, clone, "add", "--all", "--force", ".")
	git(t, clone, "commit", "--quiet", "--message", "tree")
	return clone, git(t, clone, "rev-parse", "HEAD")
}

func checkout(t *testing.T, clone, commit string) string {
	t.Helper()
	dir := t.TempDir()
	index := filepath.Join(t.TempDir(), "index")
	cmd := exec.Command("git", "-C", clone, "checkout-index", "--all", "--prefix="+dir+"/")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_INDEX_FILE=" + index, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	read := exec.Command("git", "-C", clone, "read-tree", commit)
	read.Env = cmd.Env
	if out, err := read.CombinedOutput(); err != nil {
		t.Fatalf("read-tree: %v: %s", err, out)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout-index: %v: %s", err, out)
	}
	return dir
}

func TestSeedFromGitObjectsMatchesACheckout(t *testing.T) {
	withFiles := map[string]string{}
	for name, content := range fixtureFiles {
		if strings.HasSuffix(name, "/") {
			name += "keep"
		}
		if !strings.HasPrefix(name, ".git/") {
			withFiles[name] = content
		}
	}
	outside := filepath.Join(t.TempDir(), "CODEOWNERS")
	if err := os.WriteFile(outside, []byte("* @outside\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		files, links map[string]string
	}{
		{"fixture", withFiles, nil},
		{"github first", map[string]string{".github/CODEOWNERS": "* @github\n", "CODEOWNERS": "* @root\n", "docs/CODEOWNERS": "* @docs\n"}, nil},
		{"root second", map[string]string{"CODEOWNERS": "* @root\n", "docs/CODEOWNERS": "* @docs\n", "docs/x": ""}, nil},
		{"docs third", map[string]string{"docs/CODEOWNERS": "* @docs\n"}, nil},
		{"directory is not a file", map[string]string{".github/CODEOWNERS/x": "", "CODEOWNERS": "* @root\n", "docs/x.md": ""}, nil},
		{"literal paths", map[string]string{"CODEOWNERS": "/go.mod @a\n/docs/api.md @b\n/docs/gone.md @c\n/src/ @d\n", "go.mod": "module x\n", "docs/api.md": "", "src/x.go": ""}, nil},
		{"containers", map[string]string{"internal/a/x.go": "", "internal/b/y.go": "", "cmd/tool/main.go": "", "internal/loose.go": "", "plain/deep/z.go": ""}, nil},
		{"codeowners link inside the tree", map[string]string{"docs/OWNERS": "* @linked\n", "docs/x.md": ""}, map[string]string{"CODEOWNERS": "docs/OWNERS"}},
		{"codeowners link outside the tree", map[string]string{"CODEOWNERS": "* @root\n", "docs/x.md": ""}, map[string]string{".github/CODEOWNERS": outside}},
		{"codeowners link up out of the tree", map[string]string{"CODEOWNERS": "* @root\n", "docs/x.md": ""}, map[string]string{".github/CODEOWNERS": "../../CODEOWNERS"}},
		{"directory links", map[string]string{"real/x.go": "", "CODEOWNERS": "/linked/x.go @a\n/real/x.go @b\n"}, map[string]string{"linked": "real", "only/link": "../real"}},
		{"empty tree", map[string]string{"README.md": ""}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clone, commit := commitTree(t, c.files, c.links)
			want, err := Seed(checkout(t, clone, commit))
			if err != nil {
				t.Fatal(err)
			}
			wantBytes, err := Encode(want)
			if err != nil {
				t.Fatal(err)
			}

			before := git(t, clone, "worktree", "list")
			tree, err := (&workspace.Git{Clone: clone}).Tree(context.Background(), commit)
			if err != nil {
				t.Fatal(err)
			}
			got, err := SeedFS(tree)
			if err != nil {
				t.Fatal(err)
			}
			gotBytes, err := Encode(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(gotBytes) != string(wantBytes) {
				t.Fatalf("git objects seeded\n%s\nwant the checkout's\n%s", gotBytes, wantBytes)
			}
			if after := git(t, clone, "worktree", "list"); after != before {
				t.Fatalf("worktrees changed from %q to %q", before, after)
			}
			if status := git(t, clone, "status", "--porcelain"); status != "" {
				t.Fatalf("the clone changed: %s", status)
			}
		})
	}
}

func TestSeedFromGitObjectsReadsSubmodulesAsEmptyDirectories(t *testing.T) {
	clone, _ := commitTree(t, map[string]string{"CODEOWNERS": "* @root\n", "app/x.go": ""}, nil)
	blob := git(t, clone, "rev-parse", "HEAD")
	git(t, clone, "update-index", "--add", "--cacheinfo", "160000,"+blob+",vendor")
	git(t, clone, "commit", "--quiet", "--message", "submodule")
	commit := git(t, clone, "rev-parse", "HEAD")

	want, err := Seed(checkout(t, clone, commit))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := (&workspace.Git{Clone: clone}).Tree(context.Background(), commit)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SeedFS(tree)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Encode(got)
	b, _ := Encode(want)
	if string(a) != string(b) || !strings.Contains(string(a), `"vendor"`) {
		t.Fatalf("git objects seeded\n%s\nwant the checkout's\n%s", a, b)
	}
}

func TestTreeRefusesWhatIsNotACommit(t *testing.T) {
	clone, _ := commitTree(t, map[string]string{"a/x": ""}, nil)
	g := &workspace.Git{Clone: clone}
	for _, commit := range []string{"", "-h", "0000000000000000000000000000000000000000", "no-such-ref"} {
		if _, err := g.Tree(context.Background(), commit); err == nil {
			t.Errorf("Tree(%q) succeeded", commit)
		}
	}
}
