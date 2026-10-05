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
	"time"

	"github.com/go-json-experiment/json/jsontext"
)

func checkInternalPath(name string) error {
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
	return nil
}

// checked rejects aliases inside the root as well as escapes. os.Root also
// confines the subsequent operation if an ancestor is replaced concurrently.
func (r *Repository) checked(name string) error {
	if err := checkInternalPath(name); err != nil {
		return err
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

// checkedEntry is checked for an entry of walkDir named name, listed in its
// open directory parent. The walk has already checked the entry's ancestors,
// so only the entry itself is inspected, relative to parent.
func checkedEntry(parent *os.Root, name string, entry fs.DirEntry) error {
	if entry.IsDir() {
		return nil
	}
	info, err := parent.Lstat(entry.Name())
	if os.IsNotExist(err) {
		// Git removes its temporary files while another handle walks.
		return nil
	}
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
	return r.readCheckedFile(name)
}

// readCheckedFile opens a file whose ancestors were checked by checked or walk.
// The descriptor is still confined by os.Root; the opened file must be regular
// and unaliased even if it was replaced since the walk's directory listing.
func (r *Repository) readCheckedFile(name string) ([]byte, error) {
	if err := checkInternalPath(name); err != nil {
		return nil, err
	}
	f, err := r.dir.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return readOpened(f, name)
}

// readEntry is readCheckedFile for an entry of walkDir named name, listed in
// its open directory parent.
func readEntry(parent *os.Root, name string, entry fs.DirEntry) ([]byte, error) {
	if err := checkInternalPath(name); err != nil {
		return nil, err
	}
	f, err := parent.OpenFile(entry.Name(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return readOpened(f, name)
}

// readEntryStat is readEntry that also returns the status the bytes were
// read in. The status is untrusted when the file changed during the read.
func readEntryStat(parent *os.Root, name string, entry fs.DirEntry) ([]byte, fileStat, error) {
	if err := checkInternalPath(name); err != nil {
		return nil, fileStat{}, err
	}
	f, err := parent.OpenFile(entry.Name(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fileStat{}, err
	}
	defer f.Close()
	read := time.Now()
	before, err := f.Stat()
	if err != nil {
		return nil, fileStat{}, err
	}
	if !before.Mode().IsRegular() {
		return nil, fileStat{}, fmt.Errorf("%s: expected regular file", name)
	}
	if st, ok := before.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return nil, fileStat{}, fmt.Errorf("%s: hardlink aliases are forbidden", name)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fileStat{}, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, fileStat{}, err
	}
	stat, ok := statOf(before, read)
	end, endOK := statOf(after, read)
	if !ok || !endOK || !stat.same(end) {
		stat = fileStat{}
	}
	return data, stat, nil
}

// readOpened reads and closes f, the file name, which must be regular and
// unaliased.
func readOpened(f *os.File, name string) ([]byte, error) {
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: expected regular file", name)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return nil, fmt.Errorf("%s: hardlink aliases are forbidden", name)
	}
	return io.ReadAll(f)
}
func (r *Repository) mkdir(name string) error {
	if err := r.checked(name); err != nil {
		return err
	}
	defer r.changed()
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
	defer r.changed()
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
	// Validate object names without allocating a Go value for every JSON token.
	if _, err := jsontext.NewDecoder(bytes.NewBuffer(data), jsontext.AllowInvalidUTF8(true)).ReadValue(); err != nil {
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
	case "osmia.trace.amendment":
		r = Amendment{}
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
	case "osmia.trace.priority":
		r = PriorityChange{}
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
	case Amendment:
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
	case PriorityChange:
		err := decode(data, &v)
		return v, err
	}
	panic("unreachable")
}

// treeFiles are the trace files outside .git that one walk listed, in
// lexical order, and the bytes it read from those it read.
type treeFiles struct {
	names []string
	data  map[string][]byte
}

// read returns the bytes the walk read from name, if any.
func (t *treeFiles) read(name string) ([]byte, bool) {
	if t == nil {
		return nil, false
	}
	data, ok := t.data[name]
	return data, ok
}

// walk visits the checked trace files and directories outside .git, each
// with the open directory that lists it.
func (r *Repository) walk(fn func(parent *os.Root, name string, entry fs.DirEntry) error) error {
	return r.walkDir(".", func(parent *os.Root, name string, entry fs.DirEntry) error {
		if name == ".git" {
			return fs.SkipDir
		}
		if err := checkedEntry(parent, name, entry); err != nil {
			return err
		}
		return fn(parent, name, entry)
	})
}

// walkDir visits dir and everything below it in lexical order, as fs.WalkDir
// does over r.dir, and fn may likewise return fs.SkipDir. Each directory is
// opened once, confined to the root, and fn receives it as the parent of each
// entry it lists, so an entry can be inspected relative to it rather than by
// resolving its whole path again. The parent of dir itself is nil.
func (r *Repository) walkDir(dir string, fn func(parent *os.Root, name string, entry fs.DirEntry) error) error {
	info, err := r.dir.Stat(dir)
	if err != nil {
		return err
	}
	entry := fs.FileInfoToDirEntry(info)
	if err := fn(nil, dir, entry); err != nil || !entry.IsDir() {
		if err == fs.SkipDir {
			return nil
		}
		return err
	}
	open, err := r.dir.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer open.Close()
	return walkEntries(open, dir, fn)
}

func walkEntries(dir *os.Root, name string, fn func(*os.Root, string, fs.DirEntry) error) error {
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := path.Join(name, entry.Name())
		err := fn(dir, child, entry)
		if err == fs.SkipDir {
			if entry.IsDir() {
				continue
			}
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			continue
		}
		sub, err := dir.OpenRoot(entry.Name())
		if err != nil {
			return err
		}
		err = walkEntries(sub, child, fn)
		sub.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
