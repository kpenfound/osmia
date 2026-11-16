package intake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"slices"
)

// CommitPath is a directory's entry names or a regular file's content at one
// path of a commit.
type CommitPath struct {
	Dir   bool
	Names []string
	Data  []byte
}

// ReadCommitPath reads a single path, "." for the root, from a commit's tree
// without staging the rest of it, so a read costs the same however many files
// the commit tracks. It exposes directories and regular files
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
