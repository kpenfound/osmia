package service

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
)

// imageWorktree is a worktree holding image.png, and the PNG bytes it holds.
func imageWorktree(t *testing.T) (directory string, data []byte) {
	t.Helper()
	directory = t.TempDir()
	data = attachmentPNG(t)
	intakefixture.Must(t, os.WriteFile(filepath.Join(directory, "image.png"), data, 0600))
	return directory, data
}

func TestActiveImageReadsAFileOfTheWorktree(t *testing.T) {
	t.Parallel()
	directory, data := imageWorktree(t)
	got, err := activeImage(directory, "image.png")
	intakefixture.Must(t, err)
	if !bytes.Equal(got, data) {
		t.Fatal("bytes changed")
	}
}

func TestActiveImageRefusesPathsOutsideTheWorktree(t *testing.T) {
	t.Parallel()
	directory, data := imageWorktree(t)
	outside := filepath.Join(t.TempDir(), "outside.png")
	intakefixture.Must(t, os.WriteFile(outside, data, 0600))
	intakefixture.Must(t, os.Symlink(outside, filepath.Join(directory, "escape.png")))
	for _, path := range []string{outside, "escape.png", "../outside.png"} {
		if _, err := activeImage(directory, path); err == nil {
			t.Fatalf("accepted outside: %s", path)
		}
	}
}

func TestActiveImageFollowsASymlinkThatStaysInTheWorktree(t *testing.T) {
	t.Parallel()
	directory, data := imageWorktree(t)
	intakefixture.Must(t, os.Symlink("image.png", filepath.Join(directory, "inside.png")))
	got, err := activeImage(directory, "inside.png")
	intakefixture.Must(t, err)
	if !bytes.Equal(got, data) {
		t.Fatal("in-tree bytes changed")
	}
}
