package kb

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
)

// OutputDirectory is the directory, relative to the librarian's view, that an
// extraction pass writes the complete knowledge base to: kb/entities.json and
// one kb/<subsystem>.md per subsystem.
const OutputDirectory = "output"

var subsystemPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// MaxSubsystemLength bounds a subsystem name so its document ID stays valid.
const MaxSubsystemLength = 64

// CheckSubsystem accepts a subsystem name: lowercase letters, digits and
// hyphens, starting with a letter or digit, at most MaxSubsystemLength long.
func CheckSubsystem(name string) error {
	if !subsystemPattern.MatchString(name) {
		return fmt.Errorf("subsystem name %q must match ^[a-z0-9][a-z0-9-]*$", name)
	}
	if len(name) > MaxSubsystemLength {
		return fmt.Errorf("subsystem name %q is longer than %d characters", name, MaxSubsystemLength)
	}
	return nil
}

// ProseDocument is the record ID of a subsystem's prose revisions.
func ProseDocument(subsystem string) string { return "subsystem-" + subsystem }

// Subsystem returns the subsystem a prose document path names, if any.
func Subsystem(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "kb/")
	if !ok {
		return "", false
	}
	name, ok := strings.CutSuffix(rest, ".md")
	if !ok || CheckSubsystem(name) != nil {
		return "", false
	}
	return name, true
}

// Output is the knowledge base one extraction pass produced.
type Output struct {
	Entities Map
	// Prose maps each subsystem name to its kb/<subsystem>.md content.
	Prose map[string]string
}

// Subsystems returns the produced subsystem names, sorted.
func (o Output) Subsystems() []string {
	names := make([]string, 0, len(o.Prose))
	for name := range o.Prose {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ReadOutput reads and validates an extraction pass's output directory. It
// returns every problem at once: files outside kb, nested paths, invalid
// subsystem names, empty prose, a missing entity map, and every entity map
// problem Validate reports. Symlinks and special files are refused.
func ReadOutput(dir string) (Output, error) {
	out := Output{Prose: map[string]string{}}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return out, fmt.Errorf("no output was produced: %w", err)
	}
	defer root.Close()
	var problems []error
	add := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }
	fsys := root.FS()
	top, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return out, err
	}
	for _, e := range top {
		if e.Name() != "kb" || !e.IsDir() {
			add("unexpected output entry %q: the knowledge base is written under kb/", e.Name())
		}
	}
	entries, err := fs.ReadDir(fsys, "kb")
	if err != nil {
		return out, errors.Join(append(problems, fmt.Errorf("kb/ is missing from the output"))...)
	}
	entitiesFound := false
	for _, e := range entries {
		name := e.Name()
		info, err := root.Lstat("kb/" + name)
		if err != nil {
			return out, err
		}
		if !info.Mode().IsRegular() {
			add("kb/%s is not a regular file: subsystem prose is written directly in kb/", name)
			continue
		}
		if name == "entities.json" {
			data, err := root.ReadFile("kb/" + name)
			if err != nil {
				return out, err
			}
			m, err := Parse(data)
			if err != nil {
				add("kb/entities.json: %v", err)
				continue
			}
			out.Entities, entitiesFound = m, true
			continue
		}
		subsystem, ok := strings.CutSuffix(name, ".md")
		if !ok {
			add("kb/%s is not a subsystem file: expected kb/<subsystem>.md or kb/entities.json", name)
			continue
		}
		if err := CheckSubsystem(subsystem); err != nil {
			add("kb/%s: %v", name, err)
			continue
		}
		data, err := root.ReadFile("kb/" + name)
		if err != nil {
			return out, err
		}
		if strings.TrimSpace(string(data)) == "" {
			add("kb/%s is empty", name)
			continue
		}
		out.Prose[subsystem] = string(data)
	}
	if !entitiesFound {
		add("kb/entities.json is missing from the output")
	}
	if len(out.Prose) == 0 {
		add("no kb/<subsystem>.md prose was produced")
	}
	return out, errors.Join(problems...)
}
