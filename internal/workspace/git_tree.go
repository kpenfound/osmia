package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
)

// Tree returns the files of a commit as a read-only file system over the
// clone's Git objects. No worktree is checked out and no ref or file of the
// clone changes. It reads as a checkout of the commit would: a symbolic link
// is followed while it stays inside the tree and fails otherwise, as it does
// under os.Root, and a submodule is an empty directory. The returned file
// system reads with ctx and is safe for concurrent use.
func (g *Git) Tree(ctx context.Context, commit string) (fs.FS, error) {
	if commit == "" || strings.HasPrefix(commit, "-") {
		return nil, fmt.Errorf("commit %q is not a revision", commit)
	}
	oid, err := g.run(ctx, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return nil, err
	}
	return &gitTree{git: g, ctx: ctx, root: oid, trees: map[string]map[string]treeEntry{}}, nil
}

// treeEntry is one entry of a Git tree object.
type treeEntry struct {
	name, mode, kind, oid string
}

func (e treeEntry) symlink() bool { return e.mode == "120000" }

// directory reports whether a checkout holds a directory for the entry: a
// subtree, or a submodule, which a checkout creates empty.
func (e treeEntry) directory() bool { return e.kind == "tree" || e.kind == "commit" }

func (e treeEntry) fileMode() fs.FileMode {
	switch {
	case e.directory():
		return fs.ModeDir | 0o755
	case e.symlink():
		return fs.ModeSymlink | 0o777
	case e.mode == "100755":
		return 0o755
	}
	return 0o644
}

// maxTreeLinks bounds the symbolic links one lookup follows, as os.Root does.
const maxTreeLinks = 255

type gitTree struct {
	git  *Git
	ctx  context.Context
	root string

	mu    sync.Mutex
	trees map[string]map[string]treeEntry
}

var (
	_ fs.FS         = (*gitTree)(nil)
	_ fs.StatFS     = (*gitTree)(nil)
	_ fs.ReadDirFS  = (*gitTree)(nil)
	_ fs.ReadFileFS = (*gitTree)(nil)
)

var errTreeEscape = errors.New("path escapes from the tree")

// list returns the entries of the tree object oid by name. The empty oid is
// the empty tree.
func (t *gitTree) list(oid string) (map[string]treeEntry, error) {
	if oid == "" {
		return nil, nil
	}
	t.mu.Lock()
	entries, ok := t.trees[oid]
	t.mu.Unlock()
	if ok {
		return entries, nil
	}
	out, err := t.git.runInRaw(t.ctx, t.git.Clone, nil, "ls-tree", "-z", oid)
	if err != nil {
		return nil, err
	}
	entries = map[string]treeEntry{}
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("git ls-tree: unexpected entry %q", record)
		}
		entries[name] = treeEntry{name: name, mode: fields[0], kind: fields[1], oid: fields[2]}
	}
	t.mu.Lock()
	t.trees[oid] = entries
	t.mu.Unlock()
	return entries, nil
}

func (t *gitTree) blob(oid string) ([]byte, error) {
	out, err := t.git.runInRaw(t.ctx, t.git.Clone, nil, "cat-file", "blob", oid)
	return []byte(out), err
}

// lookup resolves name to its entry, following every symbolic link on the way
// and, when follow is set, the one that names the entry itself. The root is a
// directory entry whose oid is the root tree.
func (t *gitTree) lookup(name string, follow bool) (treeEntry, error) {
	current := treeEntry{mode: "040000", kind: "tree", oid: t.root}
	if name == "." {
		return current, nil
	}
	stack := []treeEntry{current}
	todo := strings.Split(name, "/")
	links := 0
	for len(todo) > 0 {
		component := todo[0]
		todo = todo[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(stack) == 1 {
				return treeEntry{}, errTreeEscape
			}
			stack = stack[:len(stack)-1]
			continue
		}
		parent := stack[len(stack)-1]
		oid := parent.oid
		if parent.kind == "commit" {
			oid = ""
		}
		entries, err := t.list(oid)
		if err != nil {
			return treeEntry{}, err
		}
		entry, ok := entries[component]
		if !ok {
			return treeEntry{}, fs.ErrNotExist
		}
		if entry.symlink() && (follow || len(todo) > 0) {
			if links++; links > maxTreeLinks {
				return treeEntry{}, errors.New("too many levels of symbolic links")
			}
			target, err := t.blob(entry.oid)
			if err != nil {
				return treeEntry{}, err
			}
			if len(target) == 0 || target[0] == '/' {
				return treeEntry{}, errTreeEscape
			}
			todo = append(strings.Split(string(target), "/"), todo...)
			continue
		}
		if len(todo) == 0 {
			return entry, nil
		}
		if !entry.directory() {
			return treeEntry{}, fs.ErrNotExist
		}
		stack = append(stack, entry)
	}
	return stack[len(stack)-1], nil
}

func (t *gitTree) resolve(op, name string, follow bool) (treeEntry, error) {
	if !fs.ValidPath(name) {
		return treeEntry{}, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	entry, err := t.lookup(name, follow)
	if err != nil {
		return treeEntry{}, &fs.PathError{Op: op, Path: name, Err: err}
	}
	return entry, nil
}

// Stat describes name, following symbolic links as a checkout's would.
func (t *gitTree) Stat(name string) (fs.FileInfo, error) {
	entry, err := t.resolve("stat", name, true)
	if err != nil {
		return nil, err
	}
	return treeInfo{entry: entry, name: path.Base(name)}, nil
}

// ReadFile returns the content of the regular file name.
func (t *gitTree) ReadFile(name string) ([]byte, error) {
	entry, err := t.resolve("readfile", name, true)
	if err != nil {
		return nil, err
	}
	if entry.directory() {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: errors.New("is a directory")}
	}
	data, err := t.blob(entry.oid)
	if err != nil {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: err}
	}
	return data, nil
}

// ReadDir lists the directory name sorted by file name. A symbolic link
// among its entries is described, not followed.
func (t *gitTree) ReadDir(name string) ([]fs.DirEntry, error) {
	entry, err := t.resolve("readdir", name, true)
	if err != nil {
		return nil, err
	}
	if !entry.directory() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	oid := entry.oid
	if entry.kind == "commit" {
		oid = ""
	}
	entries, err := t.list(oid)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	out := make([]fs.DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, fs.FileInfoToDirEntry(treeInfo{entry: e, name: e.name}))
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

// Open opens name as a file; a directory lists its entries.
func (t *gitTree) Open(name string) (fs.File, error) {
	entry, err := t.resolve("open", name, true)
	if err != nil {
		return nil, err
	}
	info := treeInfo{entry: entry, name: path.Base(name)}
	if entry.directory() {
		entries, err := t.ReadDir(name)
		if err != nil {
			return nil, err
		}
		return &treeDirectory{info: info, entries: entries}, nil
	}
	data, err := t.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return &treeFile{info: info, Reader: bytes.NewReader(data)}, nil
}

type treeInfo struct {
	entry treeEntry
	name  string
}

func (i treeInfo) Name() string       { return i.name }
func (i treeInfo) Mode() fs.FileMode  { return i.entry.fileMode() }
func (i treeInfo) ModTime() time.Time { return time.Time{} }
func (i treeInfo) IsDir() bool        { return i.entry.directory() }
func (i treeInfo) Sys() any           { return nil }
func (i treeInfo) Size() int64        { return 0 }

type treeFile struct {
	info treeInfo
	*bytes.Reader
}

func (f *treeFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *treeFile) Close() error               { return nil }

type treeDirectory struct {
	info    treeInfo
	entries []fs.DirEntry
}

func (d *treeDirectory) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *treeDirectory) Close() error               { return nil }
func (d *treeDirectory) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: errors.New("is a directory")}
}

func (d *treeDirectory) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		out := d.entries
		d.entries = nil
		return out, nil
	}
	if len(d.entries) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(d.entries))
	out := d.entries[:n]
	d.entries = d.entries[n:]
	return out, nil
}
