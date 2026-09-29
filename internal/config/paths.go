package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Root is resolved without creating any files. Managed paths check existing
// symlinks on every call; writers must still protect against concurrent changes.
// config is the top-level configuration file, which the default root keeps
// outside its directory.
type Root struct{ directory, config string }

func (r Root) String() string { return r.directory }

// ResolveRoot uses explicit over the default root. An explicit root holds its
// own config.toml. The default root is osmia in the XDG data directory and
// reads osmia/config.toml in the XDG config directory; each XDG variable is
// used when it is absolute, otherwise ~/.local/share and ~/.config. home is
// injectable; an empty home consults os.UserHomeDir only when tilde expansion
// is needed.
func ResolveRoot(explicit, home string) (Root, error) {
	config := ""
	if explicit == "" {
		explicit = xdgDirectory("XDG_DATA_HOME", "~/.local/share", "osmia")
		c, err := resolvePath(xdgDirectory("XDG_CONFIG_HOME", "~/.config", "osmia", "config.toml"), home, "")
		if err != nil {
			return Root{}, fmt.Errorf("config: %w", err)
		}
		config = c
	}
	p, err := resolvePath(explicit, home, "")
	if err != nil {
		return Root{}, fmt.Errorf("root: %w", err)
	}
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return Root{}, fmt.Errorf("root: %s is not a directory", p)
	}
	if config == filepath.Join(p, "config.toml") {
		config = ""
	}
	return Root{p, config}, nil
}

// SelfContained reports whether the top-level file is the root's own
// config.toml, so passing the directory as an explicit root reads the same file.
func (r Root) SelfContained() bool { return r.config == "" }

// Resolve is the root these options locate, with File as its top-level file
// when set.
func (o Options) Resolve() (Root, error) {
	r, err := ResolveRoot(o.Root, o.Home)
	if err == nil && o.File != "" {
		r.config = o.File
	}
	return r, err
}

// Options pins this root's directory and top-level file for later loads.
func (r Root) Options(home string) Options {
	return Options{Root: r.directory, Home: home, File: r.config}
}
func xdgDirectory(variable, fallback string, parts ...string) string {
	base := os.Getenv(variable)
	if !filepath.IsAbs(base) {
		base = fallback
	}
	return filepath.Join(append([]string{base}, parts...)...)
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

// Config is the top-level configuration file, resolved through symlinks when
// it is outside the root.
func (r Root) Config() (string, error) {
	if r.config == "" {
		return r.managed("config.toml")
	}
	return canonical(r.config)
}
func (r Root) Runtime() (string, error) { return r.managed("runtime.json") }
func (r Root) Socket() (string, error)  { return r.managed("osmia.sock") }

// Tailnet is the embedded Tailscale node's state directory.
func (r Root) Tailnet() (string, error) { return r.managed("tailnet") }

// Notifications is the ledger of owner notifications sent to notify.webhook.
func (r Root) Notifications() (string, error) { return r.managed("notifications.json") }

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
