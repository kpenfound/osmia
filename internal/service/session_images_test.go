package service

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestActiveImageWorktreeBoundary(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	data := attachmentPNG(t)
	must(t, os.WriteFile(filepath.Join(directory, "image.png"), data, 0600))
	got, err := activeImage(directory, "image.png")
	must(t, err)
	if !bytes.Equal(got, data) {
		t.Fatal("bytes changed")
	}
	outside := filepath.Join(t.TempDir(), "outside.png")
	must(t, os.WriteFile(outside, data, 0600))
	must(t, os.Symlink(outside, filepath.Join(directory, "escape.png")))
	for _, path := range []string{outside, "escape.png", "../outside.png"} {
		if _, err := activeImage(directory, path); err == nil {
			t.Fatalf("accepted outside: %s", path)
		}
	}
	// An in-tree symlink is allowed after resolution and constrained opening.
	must(t, os.Symlink("image.png", filepath.Join(directory, "inside.png")))
	got, err = activeImage(directory, "inside.png")
	must(t, err)
	if !bytes.Equal(got, data) {
		t.Fatal("in-tree bytes changed")
	}
}
