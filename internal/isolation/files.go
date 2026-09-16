// Package isolation owns per-turn file views and execution policy for the service.
package isolation

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// Views must be rooted in service-owned storage outside the target repository.
// The source lease must exclude concurrent writers while a view is copied.
type Views struct{ Directory string }

// FileView exposes copied files, never a provider workspace or its VCS handle.
// File operations are serialized with release. OS isolation is the executor's job.
type FileView struct {
	mu        sync.Mutex
	root      *os.Root
	workspace coreadapter.Workspace
	closed    bool
}

func (v *FileView) Workspace() coreadapter.Workspace { return v.workspace }

func metadata(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".jj", ".hg", ".svn":
		return true
	}
	return false
}

func validPath(name string) error {
	if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:\x00") {
		return errors.New("file view requires a relative path without traversal")
	}
	for _, part := range strings.Split(name, "/") {
		if metadata(part) {
			return errors.New("VCS metadata is not exposed")
		}
	}
	return nil
}

// checkPath rejects even in-tree symlinks and special files. os.Root additionally
// confines each filesystem operation if an ancestor changes during resolution.
func checkPath(root *os.Root, name string, missing bool) error {
	if err := validPath(name); err != nil {
		return err
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if missing && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("symlinks and special files are not exposed")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return errors.New("file view ancestor is not a directory")
		}
	}
	return nil
}

// Create copies explicitly selected files or directories into a new private view.
// Selecting a directory excludes VCS entries at every depth; explicitly selecting
// metadata, a symlink or an unsupported file type fails the entire acquisition.
func (v Views) Create(ctx context.Context, source coreadapter.Workspace, paths []string) (_ *FileView, err error) {
	if source.Access != coreadapter.ReadOnly && source.Access != coreadapter.ReadWrite {
		return nil, errors.New("invalid view access")
	}
	base, err := filepath.EvalSymlinks(v.Directory)
	if err != nil {
		return nil, err
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	srcPath, err := filepath.EvalSymlinks(source.Directory)
	if err != nil {
		return nil, err
	}
	srcPath, err = filepath.Abs(srcPath)
	if err != nil {
		return nil, err
	}
	if within(srcPath, base) || within(base, srcPath) {
		return nil, errors.New("view storage and provider workspace must be disjoint")
	}
	src, err := os.OpenRoot(srcPath)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	dir, err := os.MkdirTemp(base, "turn-")
	if err != nil {
		return nil, err
	}
	dst, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	view := &FileView{root: dst, workspace: coreadapter.Workspace{ID: filepath.Base(dir), Directory: dir, Revision: source.Revision, Access: source.Access}}
	defer func() {
		if err != nil {
			err = errors.Join(err, view.Release(context.WithoutCancel(ctx)))
		}
	}()
	for _, selected := range paths {
		if err = checkPath(src, selected, false); err != nil {
			return nil, err
		}
		err = fs.WalkDir(src.FS(), selected, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if metadata(entry.Name()) {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if err := checkPath(src, name, false); err != nil {
				return err
			}
			if entry.IsDir() {
				return dst.MkdirAll(name, 0700)
			}
			data, err := src.ReadFile(name)
			if err != nil {
				return err
			}
			if err := dst.MkdirAll(filepath.Dir(name), 0700); err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return dst.WriteFile(name, data, 0600|(info.Mode().Perm()&0100))
		})
		if err != nil {
			return nil, err
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return view, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func (v *FileView) Read(name string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, os.ErrClosed
	}
	if err := checkPath(v.root, name, false); err != nil {
		return nil, err
	}
	return v.root.ReadFile(name)
}

func (v *FileView) Write(name string, data []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return os.ErrClosed
	}
	if v.workspace.Access != coreadapter.ReadWrite {
		return errors.New("role cannot write files")
	}
	if err := checkPath(v.root, name, true); err != nil {
		return err
	}
	if err := v.root.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	return v.root.WriteFile(name, data, 0600)
}

func (v *FileView) Release(context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.closed {
		if err := v.root.Close(); err != nil {
			return fmt.Errorf("close file view: %w", err)
		}
		v.closed = true
	}
	return os.RemoveAll(v.workspace.Directory)
}
