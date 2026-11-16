package workspace

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestTreeReadsACommitWithoutACheckout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scratch := f.scratch
	write := func(name, content string) {
		full := filepath.Join(scratch, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := func(name, target string) {
		if err := os.Symlink(target, filepath.Join(scratch, name)); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/guide.md", "guide\n")
	write("src/main.go", "package main\n")
	link("latest", "docs/guide.md")
	link("outside", "../elsewhere")
	link("absolute", "/etc/hostname")
	link("loop-a", "loop-b")
	link("loop-b", "loop-a")
	link("docs-link", "docs")
	git(t, "-C", scratch, "add", "--all")
	git(t, "-C", scratch, "commit", "--quiet", "-m", "tree")
	commit := git(t, "-C", scratch, "rev-parse", "HEAD")
	git(t, "-C", scratch, "push", "--quiet", "origin", "main")
	ctx := context.Background()
	if _, err := f.provider.Fetch(ctx, "upstream", "main"); err != nil {
		t.Fatal(err)
	}
	before := git(t, "-C", f.clone, "worktree", "list")
	entries, err := os.ReadDir(f.clone)
	if err != nil {
		t.Fatal(err)
	}

	tree, err := f.provider.Tree(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := fs.ReadDir(tree, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range listed {
		names = append(names, e.Name())
	}
	want := []string{"README", "absolute", "docs", "docs-link", "latest", "loop-a", "loop-b", "outside", "src"}
	if !slices.Equal(names, want) {
		t.Fatalf("root lists %v, want %v", names, want)
	}
	for _, e := range listed {
		if e.IsDir() != (e.Name() == "docs" || e.Name() == "src") {
			t.Errorf("%s: IsDir is %v", e.Name(), e.IsDir())
		}
	}

	data, err := fs.ReadFile(tree, "latest")
	if err != nil || string(data) != "guide\n" {
		t.Fatalf("a link inside the tree reads %q, %v", data, err)
	}
	if info, err := fs.Stat(tree, "docs-link/guide.md"); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("a path through a directory link: %v, %v", info, err)
	}
	file, err := tree.Open("src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := io.ReadAll(file); err != nil || string(opened) != "package main\n" {
		t.Fatalf("open reads %q, %v", opened, err)
	}
	file.Close()
	for _, name := range []string{"outside", "absolute", "loop-a", "missing", "docs/guide.md/x", "../README"} {
		if _, err := fs.Stat(tree, name); err == nil {
			t.Errorf("Stat(%q) succeeded", name)
		}
	}
	if _, err := fs.ReadDir(tree, "README"); err == nil {
		t.Error("ReadDir of a file succeeded")
	}
	if _, err := fs.ReadFile(tree, "docs"); err == nil {
		t.Error("ReadFile of a directory succeeded")
	}
	if _, err := tree.Open("docs"); err != nil {
		t.Errorf("Open of a directory: %v", err)
	}

	if after := git(t, "-C", f.clone, "worktree", "list"); after != before {
		t.Fatalf("worktrees changed from %q to %q", before, after)
	}
	after, err := os.ReadDir(f.clone)
	if err != nil || len(after) != len(entries) {
		t.Fatalf("the clone's directory changed: %v, %v", after, err)
	}
	if _, err := f.provider.Tree(ctx, "refs/does-not-exist"); err == nil {
		t.Fatal("Tree of an unknown revision succeeded")
	}
	var pathErr *fs.PathError
	if _, err := fs.Stat(tree, "missing"); !errors.As(err, &pathErr) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing path: %v", err)
	}
}
