// Package skills prepares the skills a role's configuration references by git
// URL as Claude Code plugin directories.
//
// TODO: busybees/core should provide a skill cache that clones references,
// refreshes them and wraps them as plugins. Remove this package once it does.
package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultRefresh is the refresh policy of a configuration that sets none.
const DefaultRefresh = "24h"

// Spec is a parsed skill reference: <git-url>[@<ref>][#<sub/dir>].
type Spec struct {
	Raw    string
	URL    string
	Ref    string
	Subdir string
	Name   string
}

// Parse parses a skill reference. A sub-directory must stay inside the
// repository.
func Parse(raw string) (Spec, error) {
	s := Spec{Raw: strings.TrimSpace(raw)}
	if s.Raw == "" {
		return s, fmt.Errorf("skill reference is empty")
	}
	rest := s.Raw
	if i := strings.Index(rest, "#"); i >= 0 {
		s.Subdir = strings.Trim(rest[i+1:], "/")
		rest = rest[:i]
		if s.Subdir == "" || slices.Contains(strings.Split(s.Subdir, "/"), "..") {
			return s, fmt.Errorf("skill %q: sub-directory must name a directory inside the repository", raw)
		}
	}
	// A trailing "@ref" is only a ref when it comes after the last path
	// separator, so scp-style git@host:org/repo URLs still work.
	if i := strings.LastIndex(rest, "@"); i > strings.LastIndexAny(rest, "/:") {
		s.Ref = rest[i+1:]
		rest = rest[:i]
	}
	s.URL = rest
	if s.URL == "" || strings.HasPrefix(s.URL, "-") || s.Ref != "" && strings.HasPrefix(s.Ref, "-") {
		return s, fmt.Errorf("skill %q has no git URL", raw)
	}
	base := strings.TrimSuffix(strings.TrimSuffix(s.URL, "/"), ".git")
	if i := strings.LastIndexAny(base, "/:"); i >= 0 {
		base = base[i+1:]
	}
	name := base
	if s.Subdir != "" {
		name += "-" + filepath.Base(s.Subdir)
	}
	s.Name = sanitizeName(name)
	if s.Name == "" {
		return s, fmt.Errorf("skill %q: cannot derive a name", raw)
	}
	return s, nil
}

// ParseRefresh parses a refresh policy: "never", "always" or a duration of at
// least zero. Empty is DefaultRefresh.
func ParseRefresh(policy string) (always bool, after time.Duration, err error) {
	switch p := strings.TrimSpace(policy); p {
	case "":
		return ParseRefresh(DefaultRefresh)
	case "never":
		return false, 0, nil
	case "always":
		return true, 0, nil
	default:
		d, err := time.ParseDuration(p)
		if err != nil || d < 0 {
			return false, 0, fmt.Errorf("refresh %q must be \"never\", \"always\" or a duration of at least zero such as \"24h\"", policy)
		}
		return false, d, nil
	}
}

// Manager clones skill references into Dir and returns plugin directories
// that expose only their skills. It implements core's agent.SkillPreparer.
type Manager struct {
	// Dir holds clones (Dir/repos) and generated plugins (Dir/plugins).
	Dir string
	// Refresh returns the current refresh policy (see ParseRefresh); nil is
	// DefaultRefresh. An invalid policy refreshes nothing.
	Refresh func() string
	// Git runs git in dir and returns its output.
	Git func(ctx context.Context, dir string, args ...string) (string, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger receives refresh warnings; nil is slog.Default().
	Logger *slog.Logger

	// mu serialises preparation: turns start concurrently and share Dir.
	mu sync.Mutex
}

// NewManager returns a manager caching under dir that runs the git on PATH
// without prompting for credentials.
func NewManager(dir string) *Manager {
	return &Manager{Dir: dir, Git: func(ctx context.Context, dir string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}}
}

// Prepare clones every reference that is missing, pulls the stale ones and
// returns their plugin directories in the same order.
func (m *Manager) Prepare(ctx context.Context, refs []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dirs := make([]string, 0, len(refs))
	for _, raw := range refs {
		spec, err := Parse(raw)
		if err != nil {
			return nil, err
		}
		dir, err := m.prepare(ctx, spec)
		if err != nil {
			return nil, fmt.Errorf("skill %s: %w", spec.Raw, err)
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

func (m *Manager) prepare(ctx context.Context, spec Spec) (string, error) {
	repo, err := m.clone(ctx, spec)
	if err != nil {
		return "", err
	}
	target := filepath.Join(repo, filepath.FromSlash(spec.Subdir))
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("sub-directory %q not found in repository", spec.Subdir)
	}
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("sub-directory %q leaves the repository", spec.Subdir)
	}
	return m.wrap(spec, resolved)
}

// repoDir is the directory a reference is cloned into.
func (m *Manager) repoDir(spec Spec) string {
	sum := sha256.Sum256([]byte(spec.URL + "@" + spec.Ref))
	return filepath.Join(m.Dir, "repos", spec.Name+"-"+hex.EncodeToString(sum[:4]))
}

// stamp is the sibling file whose modification time is the clone's last fetch.
func stamp(dir string) string { return dir + ".fetched" }

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

func (m *Manager) touch(dir string) {
	now := m.now()
	if err := os.WriteFile(stamp(dir), nil, 0o600); err == nil {
		_ = os.Chtimes(stamp(dir), now, now)
	}
}

// stale reports whether an existing clone is pulled before use.
func (m *Manager) stale(dir string) bool {
	policy := DefaultRefresh
	if m.Refresh != nil {
		policy = m.Refresh()
	}
	always, after, err := ParseRefresh(policy)
	switch {
	case err != nil:
		return false
	case always:
		return true
	case after <= 0:
		return false
	}
	info, err := os.Stat(stamp(dir))
	return err != nil || m.now().Sub(info.ModTime()) >= after
}

// clone returns the reference's clone, cloning it when it is missing and
// pulling it when it is stale. A failed pull is logged and the existing clone
// is used: a reference pinned to a tag cannot be pulled.
func (m *Manager) clone(ctx context.Context, spec Spec) (string, error) {
	dir := m.repoDir(spec)
	if isDir(filepath.Join(dir, ".git")) {
		if m.stale(dir) {
			if _, err := m.Git(ctx, dir, "pull", "--ff-only", "--quiet"); err != nil {
				m.logger().Warn("skill refresh failed", "skill", spec.Raw, "error", err)
			} else {
				m.touch(dir)
			}
		}
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	args := []string{"clone", "--depth", "1", "--quiet"}
	if spec.Ref != "" {
		args = append(args, "--branch", spec.Ref)
	}
	args = append(args, "--", spec.URL, dir)
	if _, err := m.Git(ctx, m.Dir, args...); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	m.touch(dir)
	return dir, nil
}

// wrap returns a generated plugin directory whose skills/ links to the skill
// or skills collection in target. Nothing else of the repository reaches the
// plugin, so its hooks, MCP servers, commands and agents are never loaded.
func (m *Manager) wrap(spec Spec, target string) (string, error) {
	sum := sha256.Sum256([]byte(spec.Raw))
	plugin := filepath.Join(m.Dir, "plugins", spec.Name+"-"+hex.EncodeToString(sum[:4]))
	skills := filepath.Join(plugin, "skills")
	var link, dest string
	switch {
	case isFile(filepath.Join(target, "SKILL.md")):
		link, dest = filepath.Join(skills, spec.Name), target
	case isDir(filepath.Join(target, "skills")):
		link, dest = skills, filepath.Join(target, "skills")
	default:
		return "", fmt.Errorf("%s is neither a skill (SKILL.md) nor a skills collection (skills/)", target)
	}
	manifest := filepath.Join(plugin, ".claude-plugin", "plugin.json")
	// A wrapper already pointing at dest is left alone: a running turn may be
	// using it.
	if isFile(manifest) {
		if got, err := os.Readlink(link); err == nil && got == dest {
			return plugin, nil
		}
	}
	if err := os.RemoveAll(plugin); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return "", err
	}
	if err := os.Symlink(dest, link); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(map[string]string{"name": spec.Name, "description": "Osmia skill from " + spec.Raw, "version": "0.0.0"}, "", "  ")
	if err != nil {
		return "", err
	}
	return plugin, os.WriteFile(manifest, data, 0o644)
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func sanitizeName(s string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
