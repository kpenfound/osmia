package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Root is resolved without creating any files. Managed paths check existing
// symlinks on every call; writers must still protect against concurrent changes.
type Root struct{ directory string }

func (r Root) String() string { return r.directory }

// ResolveRoot uses explicit over ~/.osmia. home is injectable; an empty home
// consults os.UserHomeDir only when tilde expansion is needed.
func ResolveRoot(explicit, home string) (Root, error) {
	if explicit == "" {
		explicit = "~/.osmia"
	}
	p, err := resolvePath(explicit, home, "")
	if err != nil {
		return Root{}, fmt.Errorf("root: %w", err)
	}
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return Root{}, fmt.Errorf("root: %s is not a directory", p)
	}
	return Root{p}, nil
}
func resolvePath(p, home, base string) (string, error) {
	if p == "" || strings.ContainsAny(p, "\x00\r\n") {
		return "", fmt.Errorf("invalid empty or control-containing path")
	}
	if strings.HasPrefix(p, "~") {
		if p != "~" && !strings.HasPrefix(p, "~/") {
			return "", fmt.Errorf("only ~/ home expansion is supported")
		}
		if home == "" {
			var err error
			home, err = os.UserHomeDir()
			if err != nil {
				return "", err
			}
		}
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("home must be absolute")
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	if !filepath.IsAbs(p) && base != "" {
		p = filepath.Join(base, p)
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return canonical(p)
}

// canonical resolves existing ancestors while permitting missing descendants.
func canonical(p string) (string, error) {
	_, err := os.Lstat(p)
	if err == nil {
		return filepath.EvalSymlinks(p)
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(p)
	if parent == p {
		return "", err
	}
	resolved, err := canonical(parent)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(resolved); err == nil && !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return filepath.Join(resolved, filepath.Base(p)), nil
}
func beneath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func (r Root) managed(parts ...string) (string, error) {
	if r.directory == "" {
		return "", fmt.Errorf("unresolved root")
	}
	p, err := canonical(filepath.Join(append([]string{r.directory}, parts...)...))
	if err != nil {
		return "", err
	}
	if !beneath(r.directory, p) {
		return "", fmt.Errorf("managed path %s escapes root %s", p, r.directory)
	}
	// Aliases inside the root could also make distinct identities share state.
	if p != filepath.Join(append([]string{r.directory}, parts...)...) {
		return "", fmt.Errorf("managed path must not contain symlink aliases: %s", p)
	}
	return p, nil
}
func (r Root) Config() (string, error)  { return r.managed("config.toml") }
func (r Root) Runtime() (string, error) { return r.managed("runtime.json") }
func (r Root) Socket() (string, error)  { return r.managed("osmia.sock") }

// Tailnet is the embedded Tailscale node's state directory.
func (r Root) Tailnet() (string, error) { return r.managed("tailnet") }

// PendingProject is the journal of one interrupted project registration.
func (r Root) PendingProject() (string, error) { return r.managed("project-add.json") }

// ProjectTrace is the project directory and dedicated trace repository.
func (r Root) ProjectTrace(id ProjectID) (string, error) {
	if err := CheckProjectIDs(id); err != nil {
		return "", err
	}
	return r.managed("projects", string(id))
}
func (r Root) ProjectConfig(id ProjectID) (string, error) {
	if err := CheckProjectIDs(id); err != nil {
		return "", err
	}
	return r.managed("projects", string(id), "config.toml")
}
func (r Root) Workstream(project ProjectID, stream WorkstreamID) (string, error) {
	if err := CheckProjectIDs(project); err != nil {
		return "", err
	}
	if err := CheckWorkstreamIDs(stream); err != nil {
		return "", err
	}
	return r.managed("projects", string(project), "workstreams", string(stream))
}
