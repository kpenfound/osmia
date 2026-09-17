package trace

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
)

// checked rejects aliases inside the root as well as escapes. os.Root also
// confines the subsequent operation if an ancestor is replaced concurrently.
func (r *Repository) checked(name string) error {
	if name != "." {
		// Internal Git paths may start with a dot; callers never supply these paths.
		if path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\\x00\r\n") {
			return fmt.Errorf("invalid trace path %q", name)
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return fmt.Errorf("invalid trace path %q", name)
			}
		}
	}
	current := ""
	for _, part := range strings.Split(name, "/") {
		current = path.Join(current, part)
		info, err := r.dir.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: symlink aliases are forbidden", current)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%s: expected a directory or regular file", current)
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && info.Mode().IsRegular() && st.Nlink > 1 {
			return fmt.Errorf("%s: hardlink aliases are forbidden", current)
		}
	}
	return nil
}

// checkedEntry is checked for an entry of fs.WalkDir, which has already
// checked the entry's ancestors, so only the entry itself is inspected.
func (r *Repository) checkedEntry(name string, entry fs.DirEntry) error {
	if entry.Type()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s: symlink aliases are forbidden", name)
	}
	if entry.IsDir() {
		return nil
	}
	info, err := r.dir.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: symlink aliases are forbidden", name)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: expected a directory or regular file", name)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Errorf("%s: hardlink aliases are forbidden", name)
	}
	return nil
}
func (r *Repository) readFile(name string) ([]byte, error) {
	if err := r.checked(name); err != nil {
		return nil, err
	}
	f, err := r.dir.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: expected regular file", name)
	}
	return io.ReadAll(f)
}
func (r *Repository) mkdir(name string) error {
	if err := r.checked(name); err != nil {
		return err
	}
	return r.dir.MkdirAll(name, 0700)
}
func syncDir(dir *os.Root, name string) error {
	f, err := dir.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	err = f.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}
func (r *Repository) writeFile(name string, data []byte) error {
	if err := r.checked(name); err != nil {
		return err
	}
	if err := r.mkdir(path.Dir(name)); err != nil {
		return err
	}
	tmp := path.Join(path.Dir(name), ".trace-"+rand.Text()+".tmp")
	f, err := r.dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer r.dir.Remove(tmp)
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = r.boundary("file:" + name + ":written")
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = r.boundary("file:" + name + ":synced")
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := r.checked(name); err != nil {
		return err
	}
	if err := r.dir.Rename(tmp, name); err != nil {
		return err
	}
	if err := r.boundary("file:" + name + ":renamed"); err != nil {
		return err
	}
	if err := syncDir(r.dir, path.Dir(name)); err != nil {
		return err
	}
	return r.boundary("file:" + name + ":directory-synced")
}

func decode(data []byte, v any) error {
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
func uniqueJSON(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				t, err := d.Token()
				if err != nil {
					return err
				}
				k, ok := t.(string)
				if !ok || seen[k] {
					return fmt.Errorf("duplicate or invalid JSON key %q", t)
				}
				seen[k] = true
				if err := uniqueJSON(d); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := uniqueJSON(d); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, err = d.Token()
	}
	return err
}
func decodeRecord(data []byte) (Record, error) {
	var marker struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, err
	}
	var r Record
	switch marker.Schema {
	case "osmia.trace.document":
		r = Document{}
	case "osmia.trace.transition":
		r = Transition{}
	case "osmia.trace.question":
		r = Question{}
	case "osmia.trace.ruling":
		r = Ruling{}
	case "osmia.trace.agent":
		r = Agent{}
	case "osmia.trace.turn-request":
		r = TurnRequest{}
	case "osmia.trace.turn-response":
		r = TurnResponse{}
	case "osmia.trace.cost":
		r = Cost{}
	case "osmia.trace.status":
		r = Status{}
	default:
		return nil, fmt.Errorf("unsupported record schema %q", marker.Schema)
	}
	// Decode into the concrete value without exposing pointers as record variants.
	switch v := r.(type) {
	case Document:
		err := decode(data, &v)
		return v, err
	case Transition:
		err := decode(data, &v)
		return v, err
	case Question:
		err := decode(data, &v)
		return v, err
	case Ruling:
		err := decode(data, &v)
		return v, err
	case Agent:
		err := decode(data, &v)
		return v, err
	case TurnRequest:
		err := decode(data, &v)
		return v, err
	case TurnResponse:
		err := decode(data, &v)
		return v, err
	case Cost:
		err := decode(data, &v)
		return v, err
	case Status:
		err := decode(data, &v)
		return v, err
	}
	panic("unreachable")
}

func (r *Repository) walk(fn func(string, fs.DirEntry) error) error {
	return fs.WalkDir(r.dir.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == ".git" {
			return fs.SkipDir
		}
		if err := r.checkedEntry(name, entry); err != nil {
			return err
		}
		return fn(name, entry)
	})
}
