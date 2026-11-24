// Package projectstore is Osmia's own store of the project registry and of
// every active project's settings.
//
// The store is the directory Root.ProjectStore names, under the Osmia root:
//
//	registry.json                 the ordered project IDs
//	settings/<id>/<number>.toml   one stored revision of a project's settings
//	lock                          the lock every write holds
//
// A revision holds the project file's TOML bytes, so DecodeProject,
// EditProject and ProjectFile.Set keep their validation and default markers.
// Revision files and the registry are published by renaming a synced
// temporary file, so a reader sees a whole record or none. The store adds
// nothing below projects/ and never reads or writes the top-level
// configuration file.
package projectstore

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/kpenfound/osmia/internal/config"
)

var (
	// ErrStale reports a pin that no longer names the current revision.
	ErrStale = errors.New("project settings changed since they were read")
	// ErrPinRequired reports a save that names no revision.
	ErrPinRequired = errors.New("a save must name the revision it builds on")
	// ErrNotFound reports a project with no stored settings.
	ErrNotFound = errors.New("project has no stored settings")
)

// Relabel returns err with the path of a *config.FieldError about project id's
// settings replaced by config.ProjectSettingsLabel: decoding stored bytes
// reports the path of the project's file, which stored settings never come
// from. Any other error is returned as it is.
func Relabel(root config.Root, id config.ProjectID, err error) error {
	var field *config.FieldError
	if err == nil || !errors.As(err, &field) {
		return err
	}
	if path, perr := root.ProjectConfig(id); perr != nil || field.Path != path {
		return err
	}
	relabeled := *field
	relabeled.Path = config.ProjectSettingsLabel(id)
	return &relabeled
}

// RepairHint tells the owner how to correct stored settings that do not load,
// naming the field the error is about when it names one.
func RepairHint(id config.ProjectID, err error) string {
	setting := "NAME"
	var field *config.FieldError
	if errors.As(err, &field) && field.Field != "" {
		setting = field.Field
	}
	return fmt.Sprintf("correct it with osmia project settings %s set %s=VALUE while the service is stopped", id, setting)
}

// LoadError is a project whose stored settings do not load. Its message is the
// loader's, naming the project, the field and the reason, followed by how to
// repair them.
type LoadError struct {
	Project config.ProjectID
	Err     error
}

func (e *LoadError) Error() string { return e.Err.Error() + "; " + RepairHint(e.Project, e.Err) }
func (e *LoadError) Unwrap() error { return e.Err }

// Revision is one stored revision of a project's settings. Pin names it: it
// changes with every recorded revision, even one with the same bytes.
type Revision struct {
	Number int
	Data   []byte
	Pin    string
}

func pinOf(number int, data []byte) string {
	return strconv.Itoa(number) + "-" + config.ProjectPin(data)
}

// Store is the store under one root. Its zero value is not usable; call Open.
// Reads take no lock and never create anything.
type Store struct {
	root config.Root
	step func(string) error
}

// Option configures Open.
type Option func(*Store)

// WithStep sets a function called after each durable step of a write, with a
// name for the step. A non-nil result stops the operation at that point and is
// returned, leaving the store as a crash there would. Tests use it to inject
// crashes.
func WithStep(step func(string) error) Option { return func(s *Store) { s.step = step } }

// Open returns the store of root. It touches no file.
func Open(root config.Root, options ...Option) *Store {
	s := &Store{root: root}
	for _, o := range options {
		o(s)
	}
	return s
}

func (s *Store) mark(name string) error {
	if s.step == nil {
		return nil
	}
	return s.step(name)
}

func (s *Store) directory() (string, error) { return s.root.ProjectStore() }

const (
	registryFile = "registry.json"
	lockFile     = "lock"
	tempPrefix   = ".tmp-"
)

var revisionName = regexp.MustCompile(`^([0-9]{8})\.toml$`)

type registry struct {
	Version  int      `json:"version"`
	Projects []string `json:"projects"`
}

func (s *Store) readRegistry() (registry, error) {
	dir, err := s.directory()
	if err != nil {
		return registry{}, err
	}
	path := filepath.Join(dir, registryFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return registry{Version: 1}, nil
	}
	if err != nil {
		return registry{}, err
	}
	var r registry
	if err := json.Unmarshal(data, &r); err != nil {
		return registry{}, fmt.Errorf("%s: %w", path, err)
	}
	if r.Version != 1 {
		return registry{}, fmt.Errorf("%s: unsupported registry version %d", path, r.Version)
	}
	if _, err := parseIDs(r.Projects); err != nil {
		return registry{}, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

func parseIDs(list []string) ([]config.ProjectID, error) {
	ids := make([]config.ProjectID, len(list))
	for i, s := range list {
		id, err := config.ParseProjectID(s)
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return ids, config.CheckProjectIDs(ids...)
}

// Projects returns the registry's project IDs in registry order: the order
// they were added in. A root without a registry has none.
func (s *Store) Projects() ([]config.ProjectID, error) {
	r, err := s.readRegistry()
	if err != nil {
		return nil, err
	}
	return parseIDs(r.Projects)
}

// Add appends id to the registry. An id already registered keeps its place.
func (s *Store) Add(id config.ProjectID) error {
	if err := config.CheckProjectIDs(id); err != nil {
		return err
	}
	return s.editRegistry(func(r *registry) {
		if !slices.Contains(r.Projects, string(id)) {
			r.Projects = append(r.Projects, string(id))
		}
	})
}

// Remove takes id off the registry. An id that is not registered changes
// nothing. Its stored revisions stay.
func (s *Store) Remove(id config.ProjectID) error {
	if err := config.CheckProjectIDs(id); err != nil {
		return err
	}
	return s.editRegistry(func(r *registry) {
		r.Projects = slices.DeleteFunc(r.Projects, func(p string) bool { return p == string(id) })
	})
}

func (s *Store) editRegistry(edit func(*registry)) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	r, err := s.readRegistry()
	if err != nil {
		return err
	}
	before := slices.Clone(r.Projects)
	edit(&r)
	if slices.Equal(before, r.Projects) {
		return nil
	}
	return s.writeRegistry(r)
}

func (s *Store) writeRegistry(r registry) error {
	dir, err := s.directory()
	if err != nil {
		return err
	}
	if r.Projects == nil {
		r.Projects = []string{}
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return s.publish(filepath.Join(dir, registryFile), append(data, '\n'), "registry-staged")
}

func (s *Store) lock() (func(), error) {
	dir, err := s.directory()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func (s *Store) publish(path string, data []byte, staged string) error {
	dir := filepath.Dir(path)
	if err := clearTemporaries(dir); err != nil {
		return err
	}
	tmp := filepath.Join(dir, tempPrefix+rand.Text())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := s.mark(staged); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDirectory(dir)
}

func clearTemporaries(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

func (s *Store) settingsDirectory(id config.ProjectID) (string, error) {
	if err := config.CheckProjectIDs(id); err != nil {
		return "", err
	}
	dir, err := s.directory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings", string(id)), nil
}

func (s *Store) numbers(id config.ProjectID) ([]int, error) {
	dir, err := s.settingsDirectory(id)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var numbers []int
	for _, e := range entries {
		if m := revisionName.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1])
			numbers = append(numbers, n)
		}
	}
	slices.Sort(numbers)
	return numbers, nil
}

func (s *Store) readRevision(id config.ProjectID, number int) (Revision, error) {
	dir, err := s.settingsDirectory(id)
	if err != nil {
		return Revision{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%08d.toml", number)))
	if err != nil {
		return Revision{}, err
	}
	return Revision{Number: number, Data: data, Pin: pinOf(number, data)}, nil
}

// Current returns the current revision of id's settings, or ErrNotFound.
func (s *Store) Current(id config.ProjectID) (Revision, error) {
	numbers, err := s.numbers(id)
	if err != nil {
		return Revision{}, err
	}
	if len(numbers) == 0 {
		return Revision{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return s.readRevision(id, numbers[len(numbers)-1])
}

// At returns revision number of id's settings, or ErrNotFound when the project
// has no such revision.
func (s *Store) At(id config.ProjectID, number int) (Revision, error) {
	numbers, err := s.numbers(id)
	if err != nil {
		return Revision{}, err
	}
	if !slices.Contains(numbers, number) {
		return Revision{}, fmt.Errorf("%w: %s has no revision %d", ErrNotFound, id, number)
	}
	return s.readRevision(id, number)
}

// Revisions returns every recorded revision of id's settings, oldest first.
func (s *Store) Revisions(id config.ProjectID) ([]Revision, error) {
	numbers, err := s.numbers(id)
	if err != nil {
		return nil, err
	}
	out := make([]Revision, 0, len(numbers))
	for _, n := range numbers {
		r, err := s.readRevision(id, n)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) record(id config.ProjectID, number int, data []byte) (Revision, error) {
	dir, err := s.settingsDirectory(id)
	if err != nil {
		return Revision{}, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Revision{}, err
	}
	path := filepath.Join(dir, fmt.Sprintf("%08d.toml", number))
	if _, err := os.Lstat(path); err == nil {
		return Revision{}, fmt.Errorf("%w: revision %d of %s exists", ErrStale, number, id)
	}
	if err := s.publish(path, data, fmt.Sprintf("revision-staged:%s:%d", id, number)); err != nil {
		return Revision{}, err
	}
	if err := s.mark(fmt.Sprintf("revision-recorded:%s:%d", id, number)); err != nil {
		return Revision{}, err
	}
	return Revision{Number: number, Data: slices.Clone(data), Pin: pinOf(number, data)}, nil
}

// WriteFirst records the first settings of a newly registered project, as
// revision 1. A project that already has settings keeps them, and its current
// revision is returned, so an interrupted registration can run this step again.
func (s *Store) WriteFirst(id config.ProjectID, data []byte) (Revision, error) {
	unlock, err := s.lock()
	if err != nil {
		return Revision{}, err
	}
	defer unlock()
	if current, err := s.Current(id); err == nil {
		return current, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Revision{}, err
	}
	return s.record(id, 1, data)
}

// Replace records data, already validated by the caller, as the next revision
// of id's settings if pin names the current revision. A pin that does not is
// ErrStale and nothing is written; no pin is ErrPinRequired.
func (s *Store) Replace(id config.ProjectID, pin string, data []byte) (Revision, error) {
	unlock, err := s.lock()
	if err != nil {
		return Revision{}, err
	}
	defer unlock()
	current, err := s.pinned(id, pin)
	if err != nil {
		return Revision{}, err
	}
	return s.record(id, current.Number+1, data)
}

func (s *Store) pinned(id config.ProjectID, pin string) (Revision, error) {
	if pin == "" {
		return Revision{}, ErrPinRequired
	}
	current, err := s.Current(id)
	if err != nil {
		return Revision{}, err
	}
	if current.Pin != pin {
		return Revision{}, ErrStale
	}
	return current, nil
}

// Save applies edit to the settings of the revision pin names, as a settings
// save does: the edit is a text edit of the stored bytes, validated by the
// rules loading applies, and a rejected setting is a *config.FieldError naming
// it. The result is recorded as the next revision. The pinned revision may
// itself fail to load, provided the edited result loads. A stale pin or a
// rejected edit writes nothing.
func (s *Store) Save(cfg *config.Config, home string, id config.ProjectID, pin string, edit config.ProjectEdit) (Revision, config.ProjectFile, error) {
	unlock, err := s.lock()
	if err != nil {
		return Revision{}, config.ProjectFile{}, err
	}
	defer unlock()
	current, err := s.pinned(id, pin)
	if err != nil {
		return Revision{}, config.ProjectFile{}, err
	}
	data, file, err := cfg.EditProject(id, home, current.Data, edit)
	if err != nil {
		return Revision{}, config.ProjectFile{}, Relabel(s.root, id, err)
	}
	revision, err := s.record(id, current.Number+1, data)
	if err != nil {
		return Revision{}, config.ProjectFile{}, err
	}
	return revision, file, nil
}

// Load decodes and validates the current revision of id's settings as
// loading a project file does, defaults and default markers included. The
// revision is returned even when it fails to load, so a caller can show its
// pin and bytes.
func (s *Store) Load(cfg *config.Config, home string, id config.ProjectID) (config.ProjectFile, Revision, error) {
	revision, err := s.Current(id)
	if err != nil {
		return config.ProjectFile{}, Revision{}, err
	}
	file, err := cfg.DecodeProject(id, home, revision.Data)
	return file, revision, Relabel(s.root, id, err)
}

// Apply returns a copy of cfg whose active projects are the registry's, in
// registry order, each loaded from its current stored revision. cfg is
// unchanged. A project whose settings do not load is a *LoadError.
func (s *Store) Apply(cfg *config.Config, home string) (*config.Config, error) {
	ids, err := s.Projects()
	if err != nil {
		return nil, err
	}
	projects := make([]config.Project, len(ids))
	for i, id := range ids {
		file, _, err := s.Load(cfg, home, id)
		if err != nil {
			return nil, &LoadError{Project: id, Err: err}
		}
		projects[i] = file.Project
	}
	return cfg.WithLoadedProjects(projects), nil
}

// LoadConfig loads the top-level configuration the options locate and applies
// the project registry of its root, as Apply does.
func LoadConfig(options config.Options) (*config.Config, error) {
	top, err := config.LoadTopLevel(options)
	if err != nil {
		return nil, err
	}
	return Open(top.Root).Apply(top, options.Home)
}
