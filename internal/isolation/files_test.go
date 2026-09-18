package isolation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	a "github.com/kpenfound/osmia/internal/coreadapter"
)

func put(t *testing.T, root, name, data string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFreshViewsExcludeMetadataAndKeepSourcePrivate(t *testing.T) {
	source, views := t.TempDir(), Views{Directory: t.TempDir()}
	put(t, source, "src/main.go", "original")
	put(t, source, "src/.git/config", "nested credential")
	put(t, source, ".git", "gitdir: /service/private")
	put(t, source, "unselected/secret", "not selected")
	var previous string
	for range 2 {
		v, err := views.Create(context.Background(), a.Workspace{Directory: source, Access: a.ReadWrite}, []string{"src"})
		if err != nil {
			t.Fatal(err)
		}
		if v.workspace.Directory == previous || v.workspace.Directory == source {
			t.Fatal("view reused source or prior view")
		}
		previous = v.workspace.Directory
		if data, err := v.Read("src/main.go"); err != nil || string(data) != "original" {
			t.Fatalf("copy: %s %v", data, err)
		}
		if err := v.Write("src/main.go", []byte("changed")); err != nil {
			t.Fatal(err)
		}
		if err := v.Write("new/file", []byte("new")); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{".git", "src/.git", "unselected"} {
			if _, err := os.Stat(filepath.Join(v.workspace.Directory, path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("exposed %s: %v", path, err)
			}
		}
		if data, err := os.ReadFile(filepath.Join(source, "src/main.go")); err != nil || string(data) != "original" {
			t.Fatal("source modified")
		}
		if err := v.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := v.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(v.workspace.Directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("view not removed")
		}
		if _, err := v.Read("src/main.go"); !errors.Is(err, os.ErrClosed) {
			t.Fatal("released view readable")
		}
	}
}

func TestViewsRejectTraversalSymlinksAndMetadataWrites(t *testing.T) {
	source, outside := t.TempDir(), t.TempDir()
	put(t, source, "src/file", "data")
	put(t, outside, "secret", "private")
	if err := os.Symlink(outside, filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src", filepath.Join(source, "internal-link")); err != nil {
		t.Fatal(err)
	}
	put(t, source, ".git/config", "private")
	views := Views{Directory: t.TempDir()}
	for _, path := range []string{"../secret", "/etc/passwd", "src/../.git", ".git", ".git/config", "escape/secret", "escape", "internal-link/file", "src\\file", "."} {
		t.Run(path, func(t *testing.T) {
			if _, err := views.Create(context.Background(), a.Workspace{Directory: source, Access: a.ReadWrite}, []string{path}); err == nil {
				t.Fatal("unsafe selection accepted")
			}
			entries, err := os.ReadDir(views.Directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed view leaked: %v %v", entries, err)
			}
		})
	}
	v, err := views.Create(context.Background(), a.Workspace{Directory: source, Access: a.ReadWrite}, []string{"src"})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Release(context.Background())
	if err := os.Symlink(outside, filepath.Join(v.workspace.Directory, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", "/tmp/escape", ".git/config", "new/.GIT", "new/.jj/file", "new/.hg/file", "new/.svn/file", "escape/secret", "escape/new"} {
		if err := v.Write(path, []byte("bad")); err == nil {
			t.Fatalf("unsafe write %s accepted", path)
		}
		if _, err := v.Read(path); err == nil {
			t.Fatalf("unsafe read %s accepted", path)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(outside, "secret")); string(data) != "private" {
		t.Fatal("outside file modified")
	}
}

// A selected directory's symlinks are left out of the view at every depth,
// whether they point inside the source, outside it or at a directory; the
// view holds the rest. Selecting one of them explicitly still fails.
func TestViewsSkipSymlinksInASelectedDirectory(t *testing.T) {
	source, outside, views := t.TempDir(), t.TempDir(), Views{Directory: t.TempDir()}
	put(t, source, "src/file", "data")
	put(t, source, "src/nested/code", "code")
	put(t, outside, "secret", "private")
	for link, target := range map[string]string{"src/readme": "file", "src/nested/escape": outside, "src/nested/up": ".."} {
		if err := os.Symlink(target, filepath.Join(source, link)); err != nil {
			t.Fatal(err)
		}
	}
	v, err := views.Create(context.Background(), a.Workspace{Directory: source, Access: a.ReadWrite}, []string{"src"})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Release(context.Background())
	var held []string
	if err := filepath.WalkDir(v.workspace.Directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(v.workspace.Directory, path)
		if entry.Type()&os.ModeSymlink != 0 {
			rel += " (symlink)"
		}
		held = append(held, filepath.ToSlash(rel))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{".", "src", "src/file", "src/nested", "src/nested/code"}; !slices.Equal(held, want) {
		t.Fatalf("the view holds %v, want %v", held, want)
	}
	for _, link := range []string{"src/readme", "src/nested/escape", "src/nested/up"} {
		if _, err := views.Create(context.Background(), a.Workspace{Directory: source, Access: a.ReadWrite}, []string{link}); err == nil || err.Error() != "symlinks and special files are not exposed" {
			t.Fatalf("explicitly selecting %s: %v", link, err)
		}
	}
}

func TestReadOnlyViewAndDisjointStorage(t *testing.T) {
	source := t.TempDir()
	put(t, source, "file", "original")
	v, err := (Views{Directory: t.TempDir()}).Create(context.Background(), a.Workspace{Directory: source, Access: a.ReadOnly}, []string{"file"})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Release(context.Background())
	if err := v.Write("file", []byte("bad")); err == nil {
		t.Fatal("read-only write accepted")
	}
	if err := v.Write("new", []byte("bad")); err == nil {
		t.Fatal("read-only creation accepted")
	}
	if _, err := (Views{Directory: source}).Create(context.Background(), a.Workspace{Directory: source, Access: a.ReadOnly}, nil); err == nil {
		t.Fatal("overlapping roots accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	views := Views{Directory: t.TempDir()}
	if _, err := views.Create(ctx, a.Workspace{Directory: source, Access: a.ReadOnly}, []string{"file"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(views.Directory)
	if len(entries) != 0 {
		t.Fatal("cancelled creation leaked view")
	}
}
