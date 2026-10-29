package intake

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
)

// copyCommit stages regular tracked files from the exact landed commit. Git
// object IDs, rather than the mutable feature worktree, fix the turn's source.
// Every blob streams through one git cat-file process, so staging a large
// commit does not start a process per file.
func CopyCommit(ctx context.Context, clone, commit, dst string) error {
	out, err := commitGit(ctx, clone, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return fmt.Errorf("list landing commit %s: %w", commit, err)
	}
	entries, err := parseCommitTree(out)
	if err != nil {
		return err
	}
	files := []commitTreeEntry{}
	var objects bytes.Buffer
	for _, e := range entries {
		if !fs.ValidPath(e.Name) {
			return errors.New("invalid landing tree entry")
		}
		if e.Kind == "blob" && e.readable() {
			files = append(files, e)
			fmt.Fprintln(&objects, e.Object)
		}
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", clone, "cat-file", "--batch")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	cmd.Stdin = &objects
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := stageBlobs(root, files, bufio.NewReader(stdout)); err != nil {
		cancel()
		cmd.Wait()
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("read landing commit %s: %w", commit, err)
	}
	return nil
}

// stageBlobs writes each file from git cat-file --batch output, which answers
// the requested objects in order.
func stageBlobs(root *os.Root, files []commitTreeEntry, batch *bufio.Reader) error {
	for _, f := range files {
		header, err := batch.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read %s: %w", f.Name, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != f.Object || fields[1] != "blob" {
			return fmt.Errorf("read %s: unexpected object %q", f.Name, strings.TrimSpace(header))
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.Name, err)
		}
		data := make([]byte, size+1)
		if _, err := io.ReadFull(batch, data); err != nil {
			return fmt.Errorf("read %s: %w", f.Name, err)
		}
		if err := root.MkdirAll(path.Dir(f.Name), 0700); err != nil {
			return err
		}
		if err := root.WriteFile(f.Name, data[:size], 0600); err != nil {
			return err
		}
	}
	return nil
}

// CommitPath is a directory's entry names or a regular file's content at one
// path of a commit.
type CommitPath struct {
	Dir   bool
	Names []string
	Data  []byte
}

// ReadCommitPath reads a single path, "." for the root, from a commit's tree
// without staging the rest of it, so a read costs the same however many files
// the commit tracks. Like CopyCommit it exposes directories and regular files
// only; a symlink or submodule path does not exist, and directory listings
// omit them.
func ReadCommitPath(ctx context.Context, clone, commit, name string) (CommitPath, error) {
	entry := commitTreeEntry{Mode: "040000", Kind: "tree", Object: commit}
	if name != "." {
		out, err := commitGit(ctx, clone, "ls-tree", "-z", "--full-tree", commit, "--", name)
		if err != nil {
			return CommitPath{}, fmt.Errorf("list %s at %s: %w", name, commit, err)
		}
		entries, err := parseCommitTree(out)
		if err != nil {
			return CommitPath{}, err
		}
		i := slices.IndexFunc(entries, func(e commitTreeEntry) bool { return e.Name == name })
		if i < 0 || !entries[i].readable() {
			return CommitPath{}, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
		}
		entry = entries[i]
	}
	if entry.Kind == "blob" {
		data, err := commitGit(ctx, clone, "cat-file", "blob", entry.Object)
		if err != nil {
			return CommitPath{}, fmt.Errorf("read %s at %s: %w", name, commit, err)
		}
		return CommitPath{Data: data}, nil
	}
	out, err := commitGit(ctx, clone, "ls-tree", "-z", entry.Object)
	if err != nil {
		return CommitPath{}, fmt.Errorf("list %s at %s: %w", name, commit, err)
	}
	entries, err := parseCommitTree(out)
	if err != nil {
		return CommitPath{}, err
	}
	names := []string{}
	for _, e := range entries {
		if e.readable() {
			names = append(names, e.Name)
		}
	}
	slices.Sort(names)
	return CommitPath{Dir: true, Names: names}, nil
}

// commitTreeEntry is one entry of git ls-tree output.
type commitTreeEntry struct {
	Mode, Kind, Object, Name string
}

func (e commitTreeEntry) readable() bool {
	return e.Kind == "tree" || e.Kind == "blob" && e.Mode != "120000"
}

func parseCommitTree(out []byte) ([]commitTreeEntry, error) {
	entries := []commitTreeEntry{}
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		meta, name, ok := bytes.Cut(raw, []byte{'\t'})
		fields := bytes.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, errors.New("invalid tree entry")
		}
		entries = append(entries, commitTreeEntry{Mode: string(fields[0]), Kind: string(fields[1]), Object: string(fields[2]), Name: string(name)})
	}
	return entries, nil
}

// commitGit runs a read-only git command in clone, isolated from the host's
// git configuration, with pathspecs taken literally.
func commitGit(ctx context.Context, clone string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", clone}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_LITERAL_PATHSPECS=1", "LC_ALL=C"}
	return cmd.Output()
}
