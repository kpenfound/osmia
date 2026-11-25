// Package filetransport moves files between the service and its clients over
// the tailnet API: a read-only git route over each registered project's clone
// and the archive of a turn's view in both directions.
package filetransport

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

// Limits bound what Unpack writes, so an archive cannot fill the disk.
type Limits struct {
	Bytes   int64
	Entries int
}

// DefaultLimits are the bounds Unpack applies.
var DefaultLimits = Limits{Bytes: 8 << 30, Entries: 1 << 20}

// ErrEscapes is an archive entry that would be written outside the
// destination root.
var ErrEscapes = errors.New("archive entry escapes the destination root")

// Pack writes the files, modes, empty directories and symlinks under root as a
// gzip-compressed tar stream, in a stable order. It refuses a special file.
func Pack(w io.Writer, root string) error {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	zip := gzip.NewWriter(w)
	archive := tar.NewWriter(zip)
	err = fs.WalkDir(dir.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()}
		switch {
		case info.IsDir():
			header.Typeflag, header.Name = tar.TypeDir, name+"/"
			return archive.WriteHeader(header)
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := dir.Readlink(name)
			if err != nil {
				return err
			}
			header.Typeflag, header.Linkname = tar.TypeSymlink, target
			return archive.WriteHeader(header)
		case info.Mode().IsRegular():
			header.Typeflag, header.Size = tar.TypeReg, info.Size()
			file, err := dir.Open(name)
			if err != nil {
				return err
			}
			defer file.Close()
			if err := archive.WriteHeader(header); err != nil {
				return err
			}
			_, err = io.Copy(archive, file)
			return err
		}
		return fmt.Errorf("%s is a special file, which a view archive does not hold", name)
	})
	if err != nil {
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return zip.Close()
}

// Unpack writes the archive in r into the existing directory dest. It refuses
// an entry whose path is absolute or holds a '..' segment, a symlink that is
// absolute or points outside dest, a hard link, a special file, a duplicate
// entry and anything that would be written through a symlink, and it never
// writes outside dest. On an error dest may hold the entries before it.
func Unpack(r io.Reader, dest string) error { return unpack(r, dest, DefaultLimits) }

type unpacked struct {
	root   *os.Root
	seen   map[string]bool
	dirs   []dirMode
	limits Limits
	bytes  int64
}

type dirMode struct {
	name string
	mode fs.FileMode
}

func unpack(r io.Reader, dest string, limits Limits) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	zip, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("view archive: %w", err)
	}
	defer zip.Close()
	u := &unpacked{root: root, seen: map[string]bool{}, limits: limits}
	archive := tar.NewReader(zip)
	for entries := 0; ; entries++ {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("view archive: %w", err)
		}
		if entries >= limits.Entries {
			return fmt.Errorf("view archive holds more than %d entries", limits.Entries)
		}
		if err := u.entry(header, archive); err != nil {
			return fmt.Errorf("view archive entry %q: %w", header.Name, err)
		}
	}
	slices.SortFunc(u.dirs, func(a, b dirMode) int { return strings.Count(b.name, "/") - strings.Count(a.name, "/") })
	for _, d := range u.dirs {
		if err := root.Chmod(d.name, d.mode); err != nil {
			return err
		}
	}
	return nil
}

func (u *unpacked) entry(header *tar.Header, body io.Reader) error {
	if header.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}
	name, err := entryName(header.Name)
	if err != nil {
		return err
	}
	if name == "." {
		if header.Typeflag != tar.TypeDir {
			return errors.New("the root is not a directory")
		}
		return nil
	}
	if u.seen[name] {
		return errors.New("duplicate entry")
	}
	u.seen[name] = true
	if err := u.ensureParents(name); err != nil {
		return err
	}
	perm := fs.FileMode(header.Mode) & fs.ModePerm
	switch header.Typeflag {
	case tar.TypeDir:
		if err := u.root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		u.dirs = append(u.dirs, dirMode{name, perm})
		return nil
	case tar.TypeSymlink:
		if err := checkLink(name, header.Linkname); err != nil {
			return err
		}
		return u.root.Symlink(header.Linkname, name)
	case tar.TypeReg:
		if header.Size < 0 || header.Size > u.limits.Bytes-u.bytes {
			return fmt.Errorf("view archive holds more than %d bytes", u.limits.Bytes)
		}
		u.bytes += header.Size
		file, err := u.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(file, body)
		if err = errors.Join(err, file.Chmod(perm), file.Close()); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("entry type %q is not a file, directory or symlink", header.Typeflag)
}

func entryName(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "\\") {
		return "", ErrEscapes
	}
	name := strings.TrimSuffix(strings.TrimPrefix(raw, "./"), "/")
	if name == "" || name == "." {
		return ".", nil
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", ErrEscapes
		}
	}
	if !fs.ValidPath(name) {
		return "", errors.New("the path is not a clean relative path")
	}
	return name, nil
}

// checkLink accepts a symlink only when its target is relative and stays
// inside the root without passing through a '..' that follows another
// segment, which would resolve differently once a symlink is in the way.
func checkLink(name, target string) error {
	if target == "" || strings.ContainsRune(target, 0) || path.IsAbs(target) || strings.HasPrefix(target, "\\") {
		return ErrEscapes
	}
	depth := strings.Count(name, "/")
	named := false
	for _, part := range strings.Split(target, "/") {
		switch part {
		case "", ".":
		case "..":
			if named {
				return ErrEscapes
			}
			depth--
			if depth < 0 {
				return ErrEscapes
			}
		default:
			named = true
		}
	}
	return nil
}

func (u *unpacked) ensureParents(name string) error {
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		info, err := u.root.Lstat(parent)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := u.root.Mkdir(parent, 0o700); err != nil {
				return err
			}
		case err != nil:
			return err
		case !info.IsDir():
			return fmt.Errorf("%s is not a directory, so nothing is written below it", parent)
		}
	}
	return nil
}
