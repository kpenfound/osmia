// Package skills parses skill references and refresh policies and prepares
// the referenced skills as plugin directories through agentcore's skills
// manager.
package skills

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	coreskills "github.com/kpenfound/agentcore/skills"
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

// Manager prepares skill references as plugin directories under Dir that
// expose only their skills, running agentcore's skills manager with the git
// runner, clock and refresh policy configured here. It implements
// agent.SkillPreparer.
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

	mu sync.Mutex
}

// NewManager returns a manager caching under dir that runs the git on PATH
// without prompting for credentials.
func NewManager(dir string) *Manager {
	return &Manager{Dir: dir, Git: defaultGit}
}

func defaultGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Prepare clones every reference that is missing, pulls the stale ones and
// returns their plugin directories in the same order. Every call builds a
// core skills.Manager from the current Dir, Git, Now and Refresh, so a
// policy change the caller's Refresh closure observes takes effect on the
// next call.
func (m *Manager) Prepare(ctx context.Context, refs []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cm := coreskills.NewManager(m.Dir, m.Logger)
	cm.SkillsOnly = true
	cm.Git = m.Git
	cm.Now = m.Now
	cm.SetRefresh(m.refreshPolicy())
	return cm.Prepare(ctx, refs)
}

func (m *Manager) refreshPolicy() coreskills.RefreshPolicy {
	policy := DefaultRefresh
	if m.Refresh != nil {
		policy = m.Refresh()
	}
	always, after, err := ParseRefresh(policy)
	switch {
	case err != nil:
		return coreskills.RefreshNever
	case always:
		return coreskills.RefreshAlways
	case after <= 0:
		return coreskills.RefreshNever
	default:
		return coreskills.RefreshEvery(after)
	}
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
