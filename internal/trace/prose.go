package trace

import (
	"fmt"
	"sort"
	"strings"
)

// ProsePath returns the project path of a subsystem's knowledge-base prose.
func ProsePath(subsystem string) string { return "kb/" + subsystem + ".md" }

// RecordPath returns the trace file that holds a record's revisions.
func RecordPath(v Record) string { return recordPath(v) }

// Prose returns the latest revision of kb/<subsystem>.md. A subsystem with
// no revision, or whose latest revision is empty, is fs.ErrNotExist.
func (r *Repository) Prose(subsystem string) (string, error) {
	name := ProsePath(subsystem)
	if subsystem == "" || strings.Contains(subsystem, "/") || documentPath(name, false) != nil {
		return "", fmt.Errorf("invalid subsystem name %q", subsystem)
	}
	data, err := r.file(name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Subsystems returns the sorted names of the subsystems whose latest
// kb/<subsystem>.md revision is not empty.
func (r *Repository) Subsystems() ([]string, error) {
	records, err := r.records(func(v Record) bool {
		d, ok := v.(Document)
		return ok && d.Workstream == "" && prose(d.Path)
	})
	if err != nil {
		return nil, err
	}
	latest := map[string]string{}
	for _, v := range records {
		d := v.(Document)
		latest[strings.TrimSuffix(strings.TrimPrefix(d.Path, "kb/"), ".md")] = d.Content
	}
	names := []string{}
	for name, content := range latest {
		if content != "" && name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
