package trace

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// ProsePath returns the project path of a subsystem's knowledge-base prose.
func ProsePath(subsystem string) string { return "kb/" + subsystem + ".md" }

// RecordPath returns the trace file that holds a record's revisions.
func RecordPath(v Record) string { return recordPath(v) }

// Prose reads kb/<subsystem>.md as it is on disk now. A missing file is
// reported as fs.ErrNotExist.
func (r *Repository) Prose(subsystem string) (string, error) {
	name := ProsePath(subsystem)
	if subsystem == "" || strings.Contains(subsystem, "/") || documentPath(name, false) != nil {
		return "", fmt.Errorf("invalid subsystem name %q", subsystem)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := r.readFile(name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Subsystems returns the sorted subsystem names of the kb/<subsystem>.md files
// on disk, whatever their file type, so Prose refuses a symlinked or irregular
// one. Hidden files and names that are not valid document paths are skipped.
func (r *Repository) Subsystems() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checked("kb"); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(r.dir.FS(), "kb")
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || name == "" || documentPath(ProsePath(name), false) != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
