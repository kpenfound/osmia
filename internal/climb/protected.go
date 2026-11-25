package climb

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Paths every Hill Climb protects: the Dagger workspace configuration and its
// lock file.
var protectedFiles = []string{"dagger.toml", "dagger.lock"}

// CleanPath returns p as a clean repository-relative path, or an error when
// it is empty, absolute or leaves the repository.
func CleanPath(p string) (string, error) {
	clean := path.Clean(strings.TrimSpace(p))
	switch {
	case strings.TrimSpace(p) == "" || clean == ".":
		return "", fmt.Errorf("path %q is empty", p)
	case strings.HasPrefix(clean, "/"):
		return "", fmt.Errorf("path %q is absolute", p)
	case clean == ".." || strings.HasPrefix(clean, "../"):
		return "", fmt.Errorf("path %q leaves the repository", p)
	case strings.ContainsAny(clean, "\\\x00\r\n"):
		return "", fmt.Errorf("path %q has an unsupported character", p)
	}
	return clean, nil
}

// ProtectedPaths returns the paths a climb freezes: dagger.toml, dagger.lock,
// the dev module's source directory, the declared fixtures and any paths the
// climb spec adds. A path covers itself and everything beneath it. The result
// is clean, sorted and free of duplicates.
func ProtectedPaths(devModule string, fixtures, added []string) ([]string, error) {
	all := slices.Clone(protectedFiles)
	if devModule != "" {
		all = append(all, devModule)
	}
	all = append(append(all, fixtures...), added...)
	out := make([]string, 0, len(all))
	for _, p := range all {
		clean, err := CleanPath(p)
		if err != nil {
			return nil, err
		}
		out = append(out, clean)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// DevModuleSource returns the source directory of the module that
// dagger.toml marks `main = true`, the project's dev module.
func DevModuleSource(daggerToml []byte) (string, error) {
	var config struct {
		Modules map[string]struct {
			Source string `toml:"source"`
			Main   bool   `toml:"main"`
		} `toml:"modules"`
	}
	if err := toml.Unmarshal(daggerToml, &config); err != nil {
		return "", fmt.Errorf("dagger.toml: %w", err)
	}
	var found []string
	for name, m := range config.Modules {
		if m.Main {
			found = append(found, name)
		}
	}
	slices.Sort(found)
	switch len(found) {
	case 0:
		return "", fmt.Errorf("dagger.toml marks no module main = true")
	case 1:
		source := config.Modules[found[0]].Source
		if source == "" {
			return "", fmt.Errorf("the main module %q has no source", found[0])
		}
		return source, nil
	default:
		return "", fmt.Errorf("dagger.toml marks several modules main = true: %s", strings.Join(found, ", "))
	}
}

// ProtectedError is the refusal of an attempt whose diff touches protected
// paths.
type ProtectedError struct {
	// Touched lists the changed paths that lie in a protected path.
	Touched []string
}

func (e *ProtectedError) Error() string {
	return "the attempt changes protected paths: " + strings.Join(e.Touched, ", ")
}

// Gate refuses a diff that touches a protected path. changed lists the paths
// the diff changes, with both sides of a rename. It returns a *ProtectedError
// listing every touched path, or nil when none is protected.
func Gate(protected, changed []string) error {
	var touched []string
	for _, c := range changed {
		clean := path.Clean(strings.TrimPrefix(strings.TrimSpace(c), "./"))
		for _, p := range protected {
			if clean == p || strings.HasPrefix(clean, p+"/") {
				touched = append(touched, clean)
				break
			}
		}
	}
	if len(touched) == 0 {
		return nil
	}
	slices.Sort(touched)
	return &ProtectedError{Touched: slices.Compact(touched)}
}
