package intake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
)

// copyCommit stages regular tracked files from the exact landed commit. Git
// object IDs, rather than the mutable feature worktree, fix the turn's source.
func CopyCommit(ctx context.Context, clone, commit, dst string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", clone, "ls-tree", "-r", "-z", "--full-tree", commit)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("list landing commit %s: %w", commit, err)
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, entry := range bytes.Split(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		meta, name, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok || !fs.ValidPath(string(name)) {
			return errors.New("invalid landing tree entry")
		}
		fields := bytes.Fields(meta)
		if len(fields) != 3 {
			return errors.New("invalid landing tree metadata")
		}
		if string(fields[1]) != "blob" || string(fields[0]) == "120000" {
			continue
		}
		cmd := exec.CommandContext(ctx, "git", "-C", clone, "cat-file", "blob", string(fields[2]))
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
		data, err := cmd.Output()
		if err != nil {
			return err
		}
		if err := root.MkdirAll(path.Dir(string(name)), 0700); err != nil {
			return err
		}
		if err := root.WriteFile(string(name), data, 0600); err != nil {
			return err
		}
	}
	return nil
}
