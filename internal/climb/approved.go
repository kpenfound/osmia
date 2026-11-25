package climb

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Restorer puts the approved copy of the protected paths into a fresh export
// before each measurement.
type Restorer interface {
	Restore(ctx context.Context, dir string) error
}

// ApprovedFile is one file of the approved copy. A non-empty Link makes it a
// symbolic link and Content is then unused.
type ApprovedFile struct {
	Path    string      `json:"path"`
	Mode    fs.FileMode `json:"mode"`
	Content []byte      `json:"content,omitempty"`
	Link    string      `json:"link,omitempty"`
}

// ApprovedCopy is the frozen copy of the protected paths the owner approved.
// Restoring it replaces whatever an export holds at those paths with exactly
// these files.
type ApprovedCopy struct {
	Paths []string       `json:"paths"`
	Files []ApprovedFile `json:"files"`
}

// Snapshot copies the files under paths in dir. A path that does not exist
// contributes no file, so restoring it later deletes whatever appears there.
func Snapshot(dir string, paths []string) (ApprovedCopy, error) {
	approved := ApprovedCopy{Paths: slices.Clone(paths)}
	for _, p := range paths {
		clean, err := CleanPath(p)
		if err != nil {
			return ApprovedCopy{}, err
		}
		root := filepath.Join(dir, filepath.FromSlash(clean))
		err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && name == root {
					return nil
				}
				return err
			}
			if entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(dir, name)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			file := ApprovedFile{Path: filepath.ToSlash(rel), Mode: info.Mode().Perm()}
			switch {
			case info.Mode()&fs.ModeSymlink != 0:
				if file.Link, err = os.Readlink(name); err != nil {
					return err
				}
			case info.Mode().IsRegular():
				if file.Content, err = os.ReadFile(name); err != nil {
					return err
				}
			default:
				return fmt.Errorf("protected file %s is neither a regular file nor a link", rel)
			}
			approved.Files = append(approved.Files, file)
			return nil
		})
		if err != nil {
			return ApprovedCopy{}, err
		}
	}
	slices.SortFunc(approved.Files, func(a, b ApprovedFile) int { return strings.Compare(a.Path, b.Path) })
	approved.Files = slices.CompactFunc(approved.Files, func(a, b ApprovedFile) bool { return a.Path == b.Path })
	return approved, nil
}

// Restore replaces every protected path in dir with the approved copy: what
// the export holds there is removed first, then the approved files are
// written. It refuses to follow a symbolic link out of dir.
func (c ApprovedCopy) Restore(_ context.Context, dir string) error {
	for _, p := range c.Paths {
		clean, err := CleanPath(p)
		if err != nil {
			return err
		}
		target, err := confined(dir, clean)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("remove %s: %w", clean, err)
		}
	}
	for _, f := range c.Files {
		clean, err := CleanPath(f.Path)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(c.Paths, func(p string) bool { return clean == p || strings.HasPrefix(clean, p+"/") }) {
			return fmt.Errorf("approved file %s lies in no protected path", clean)
		}
		target, err := confined(dir, clean)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if f.Link != "" {
			if err := os.Symlink(f.Link, target); err != nil {
				return err
			}
			continue
		}
		if err := os.WriteFile(target, f.Content, f.Mode|0o600); err != nil {
			return err
		}
		if err := os.Chmod(target, f.Mode); err != nil {
			return err
		}
	}
	return nil
}

// confined joins rel to dir after checking that no directory on the way is a
// symbolic link, which could lead a write out of the export.
func confined(dir, rel string) (string, error) {
	current := dir
	parts := strings.Split(path.Clean(rel), "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("export path %s passes through a symbolic link", rel)
		}
	}
	return filepath.Join(current, parts[len(parts)-1]), nil
}
