package runtime

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

type fileOps struct {
	encode  func(State) ([]byte, error)
	write   func(*os.File, []byte) (int, error)
	sync    func(*os.File) error
	rename  func(*os.Root, string, string) error
	syncDir func(*os.File) error
}

func defaultFileOps() fileOps {
	return fileOps{
		encode: func(st State) ([]byte, error) { return json.MarshalIndent(st, "", "  ") },
		write:  (*os.File).Write, sync: (*os.File).Sync, rename: (*os.Root).Rename, syncDir: syncDirectory,
	}
}
func syncDirectory(f *os.File) error {
	err := f.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}
func readRuntime(root *os.Root) ([]byte, error) {
	f, err := root.OpenFile("runtime.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("runtime.json must be a regular file")
	}
	return io.ReadAll(f)
}
func (s *Store) stage(data []byte) (string, error) {
	name := fmt.Sprintf(".runtime-%x.tmp", rand.Text())
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			s.root.Remove(name)
		}
	}()
	n, err := s.ops.write(f, data)
	if err != nil {
		return "", err
	}
	if n != len(data) {
		return "", io.ErrShortWrite
	}
	if err = s.ops.sync(f); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}
func (s *Store) checkDisk() error {
	previous, err := readRuntime(s.root)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if (err == nil) != (s.disk != nil) || !bytes.Equal(previous, s.disk) {
		return ErrConflict
	}
	return nil
}
func (s *Store) persist(data []byte) error {
	if err := s.checkDisk(); err != nil {
		return err
	}

	next, err := s.stage(data)
	if err != nil {
		return err
	}
	defer s.root.Remove(next)
	// Keep a flushed rollback copy until the replacement directory entry is durable.
	backup := ""
	if s.disk != nil {
		backup, err = s.stage(s.disk)
		if err != nil {
			return err
		}
		defer func() {
			if backup != "" {
				s.root.Remove(backup)
			}
		}()
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = s.ops.rename(s.root, next, "runtime.json"); err != nil {
		return err
	}
	if err = s.ops.syncDir(dir); err != nil {
		var restore error
		if backup != "" {
			restore = s.root.Rename(backup, "runtime.json")
		} else {
			restore = s.root.Remove("runtime.json")
		}
		if restore == nil {
			restore = syncDirectory(dir)
		}
		if restore != nil {
			recovery := backup
			backup = "" // Preserve the recovery copy if restoration failed.
			return errors.Join(err, fmt.Errorf("runtime rollback failed; disk requires reconciliation (backup %q): %w", recovery, restore))
		}
		return err
	}
	return nil
}
