package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// ProjectSettings lists the settings of a project configuration file that
// have an effect, in display order. hearsay_entities is the mapping whose
// entries are set and cleared one by one.
var ProjectSettings = []string{"name", "upstream", "fork", "clone", "base_branch", "landing", "classifier", "upstream_rebase", "pull_request_poll", "checks_timeout", "capacity.per_workstream", "capacity.git_operations", "hearsay_scope", "hearsay_declare_repo", "hearsay_entities"}

// ProjectPin identifies the exact bytes of a project configuration file.
func ProjectPin(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ProjectFile is one project configuration file's bytes decoded and
// validated as the loader reads them.
type ProjectFile struct {
	Project Project
	md      toml.MetaData
}

// Set reports whether the file sets the setting. A setting the file leaves
// out takes its default, or for capacity.per_workstream the top-level value.
func (f ProjectFile) Set(setting string) bool {
	if setting == "hearsay_entities" {
		return len(f.Project.HearsayEntities) > 0
	}
	return f.md.IsDefined(strings.Split(setting, ".")...)
}

// DecodeProject decodes and validates the bytes of project id's configuration
// file as loading does, including the classifier check against c's profiles,
// without reading the disk. The receiver is unchanged.
func (c *Config) DecodeProject(id ProjectID, home string, data []byte) (ProjectFile, error) {
	p, md, err := loadProjectBytes(c.Root, id, home, c.Capacity.PerWorkstream, data)
	if err != nil {
		return ProjectFile{}, err
	}
	if err := c.validateClassifier(p); err != nil {
		return ProjectFile{}, err
	}
	return ProjectFile{Project: p, md: md}, nil
}

// WithLoadedProject returns a copy of c in which the active project p replaces
// the one with its ID. Every other project and setting is as c has it.
func (c *Config) WithLoadedProject(p Project) *Config {
	out := *c
	out.Projects = slices.Clone(c.Projects)
	for i := range out.Projects {
		if out.Projects[i].ID == p.ID {
			out.Projects[i] = p
		}
	}
	if c.Project.ID == p.ID {
		out.Project = p
	}
	return &out
}

// ProjectEdit changes some settings of one project's configuration file.
// Strings, Ints and Bools set settings by name; Entities sets hearsay_entities
// entries. Clear removes settings, and ClearEntities removes entries, so a
// default or inherited value applies again.
type ProjectEdit struct {
	Strings       map[string]string
	Ints          map[string]int
	Bools         map[string]bool
	Entities      map[string]string
	Clear         []string
	ClearEntities []string
}

// Settings that hold a string or an integer, and those that must stay set.
var (
	stringSettings   = []string{"name", "upstream", "fork", "clone", "base_branch", "landing", "classifier", "upstream_rebase", "pull_request_poll", "checks_timeout", "hearsay_scope"}
	intSettings      = []string{"capacity.per_workstream", "capacity.git_operations"}
	boolSettings     = []string{"hearsay_declare_repo"}
	requiredSettings = []string{"upstream", "clone"}
	// optionalSettings have an empty string for a default: an empty value
	// clears them.
	optionalSettings = []string{"name", "fork", "classifier", "hearsay_scope"}
)

// EditProject applies edit to the pinned bytes of project id's file as a text
// edit: only the changed keys are set, added or removed, and every other byte
// stays as it was. It returns the new bytes with their decoded form, which
// has passed the validation loading applies. A rejected setting is a
// *FieldError naming it.
func (c *Config) EditProject(id ProjectID, home string, pinned []byte, edit ProjectEdit) ([]byte, ProjectFile, error) {
	path, err := c.Root.ProjectConfig(id)
	if err != nil {
		return nil, ProjectFile{}, err
	}
	var edits []keyEdit
	seen := map[string]bool{}
	touch := func(name string) error {
		if seen[name] {
			return fieldError(path, name, "is changed more than once")
		}
		seen[name] = true
		return nil
	}
	for _, name := range sortedKeys(edit.Strings) {
		if !slices.Contains(stringSettings, name) {
			return nil, ProjectFile{}, fieldError(path, name, "is not a string setting of the project file")
		}
		if err := touch(name); err != nil {
			return nil, ProjectFile{}, err
		}
		value := edit.Strings[name]
		if value == "" && slices.Contains(optionalSettings, name) {
			edits = append(edits, keyEdit{path: []string{name}, remove: true})
			continue
		}
		edits = append(edits, keyEdit{path: []string{name}, value: value})
	}
	for _, name := range sortedKeys(edit.Ints) {
		if !slices.Contains(intSettings, name) {
			return nil, ProjectFile{}, fieldError(path, name, "is not an integer setting of the project file")
		}
		if err := touch(name); err != nil {
			return nil, ProjectFile{}, err
		}
		edits = append(edits, keyEdit{path: strings.Split(name, "."), value: edit.Ints[name]})
	}
	for _, name := range sortedKeys(edit.Bools) {
		if !slices.Contains(boolSettings, name) {
			return nil, ProjectFile{}, fieldError(path, name, "is not a true or false setting of the project file")
		}
		if err := touch(name); err != nil {
			return nil, ProjectFile{}, err
		}
		edits = append(edits, keyEdit{path: []string{name}, value: edit.Bools[name]})
	}
	for _, name := range edit.Clear {
		if !slices.Contains(stringSettings, name) && !slices.Contains(intSettings, name) && !slices.Contains(boolSettings, name) {
			return nil, ProjectFile{}, fieldError(path, name, "is not a setting that can be cleared")
		}
		if slices.Contains(requiredSettings, name) {
			return nil, ProjectFile{}, fieldError(path, name, "is required and has no default; set a value instead")
		}
		if err := touch(name); err != nil {
			return nil, ProjectFile{}, err
		}
		edits = append(edits, keyEdit{path: strings.Split(name, "."), remove: true})
	}
	entities := map[string]bool{}
	for _, entity := range sortedKeys(edit.Entities) {
		if strings.TrimSpace(entity) == "" {
			return nil, ProjectFile{}, fieldError(path, "hearsay_entities", "must map local entity IDs to Hearsay entity ids")
		}
		entities[entity] = true
		edits = append(edits, keyEdit{path: []string{"hearsay_entities", entity}, value: edit.Entities[entity]})
	}
	for _, entity := range edit.ClearEntities {
		if entities[entity] {
			return nil, ProjectFile{}, fieldError(path, "hearsay_entities", "changes the entry "+fmt.Sprintf("%q", entity)+" more than once")
		}
		entities[entity] = true
		edits = append(edits, keyEdit{path: []string{"hearsay_entities", entity}, remove: true})
	}
	text, err := editTOML(string(pinned), edits)
	if err != nil {
		return nil, ProjectFile{}, &FieldError{Path: path, Reason: "cannot be edited as text; edit the file by hand", Err: err}
	}
	file, err := c.DecodeProject(id, home, []byte(text))
	if err != nil {
		return nil, ProjectFile{}, err
	}
	return []byte(text), file, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ReplaceFile replaces the existing regular file at path atomically, keeping
// its mode. A failure leaves the old bytes and no temporary file; the one
// failure that may follow the replacement is syncing the directory, so a
// caller that must know reads the file back.
func ReplaceFile(path string, data []byte) error { return replaceFile(path, data) }
