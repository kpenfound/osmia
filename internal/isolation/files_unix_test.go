//go:build unix

package isolation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	a "github.com/kpenfound/osmia/internal/coreadapter"
)

func TestViewsStripGroupWorldModesAndRejectSpecialFiles(t *testing.T) {
	source, views := t.TempDir(), Views{Directory: t.TempDir()}
	put(t, source, "src/tool.sh", "#!/bin/sh")
	put(t, source, "src/data", "data")
	for name, mode := range map[string]os.FileMode{"src/tool.sh": 0755, "src/data": 0644} {
		if err := os.Chmod(filepath.Join(source, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	v, err := views.Create(context.Background(), a.Workspace{Directory: source, Access: a.ReadOnly}, []string{"src"})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"src/tool.sh": 0700, "src/data": 0600} {
		info, err := os.Stat(filepath.Join(v.workspace.Directory, name))
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode %v, want %v (%v)", name, info.Mode().Perm(), want, err)
		}
	}
	if err := v.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(source, "src/pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, selection := range [][]string{{"src/pipe"}, {"src/data", "src/pipe"}} {
		if _, err := views.Create(context.Background(), a.Workspace{Directory: source, Access: a.ReadOnly}, selection); err == nil || err.Error() != "symlinks and special files are not exposed" {
			t.Fatalf("special file accepted through %v: %v", selection, err)
		}
		entries, err := os.ReadDir(views.Directory)
		if err != nil || len(entries) != 0 {
			t.Fatalf("failed view leaked: %v %v", entries, err)
		}
	}
	// Within a selected directory the special file is left out.
	v, err = views.Create(context.Background(), a.Workspace{Directory: source, Access: a.ReadOnly}, []string{"src"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(v.workspace.Directory, "src/pipe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the view holds the special file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(v.workspace.Directory, "src/data")); err != nil {
		t.Fatalf("the view lacks src/data: %v", err)
	}
	if err := v.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, "src/pipe")); errors.Is(err, os.ErrNotExist) {
		t.Fatal("source special file removed")
	}
}
