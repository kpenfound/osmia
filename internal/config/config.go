// Package config loads the declarative M1 configuration without creating state
// or starting execution. A successful load is not an execution capability grant.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

type Options struct{ Root, Home string }
type Config struct {
	Root           Root               `toml:"-"`
	Version        int                `toml:"version"`
	ActiveProjects []string           `toml:"active_projects"`
	Listen         Listen             `toml:"listen"`
	Capacity       Capacity           `toml:"capacity"`
	Profiles       map[string]Profile `toml:"profiles"`
	Roles          map[string]Role    `toml:"roles"`
	Shed           Shed               `toml:"shed"`
	Project        Project            `toml:"-"`
}
type Listen struct {
	Socket string `toml:"socket"`
}
type Capacity struct {
	Masons        int `toml:"masons"`
	Reviewers     int `toml:"reviewers"`
	Committee     int `toml:"committee"`
	PerWorkstream int `toml:"per_workstream"`
}
type Shed struct {
	MaxRounds  int `toml:"max_rounds"`
	MaxBounces int `toml:"max_bounces"`
}
type Profile struct {
	Agent    string `toml:"agent"`
	Model    string `toml:"model"`
	Effort   string `toml:"effort"`
	Fallback string `toml:"fallback"`
	Timeout  string `toml:"timeout"`
	MaxTurns int    `toml:"max_turns"`
}
type Role struct {
	Profile string `toml:"profile"`
	Sandbox string `toml:"sandbox"`
	Image   string `toml:"image"`
}
type Project struct {
	ID         ProjectID       `toml:"-"`
	Version    int             `toml:"version"`
	Name       string          `toml:"name"`
	Upstream   string          `toml:"upstream"`
	Fork       string          `toml:"fork"`
	Clone      string          `toml:"clone"`
	BaseBranch string          `toml:"base_branch"`
	Landing    string          `toml:"landing"`
	Capacity   ProjectCapacity `toml:"capacity"`
}
type ProjectCapacity struct {
	PerWorkstream int `toml:"per_workstream"`
}

var roleNames = []string{"architect", "chief_of_staff", "committee", "foreman", "librarian", "mason", "reviewer"}
var profileName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func fieldError(path, field, reason string) error {
	return fmt.Errorf("%s: %s: %s", path, field, reason)
}

// Load returns nil on every failure. Both files are required, and only the
// explicitly active project is read; archived directories are not activated.
func Load(options Options) (*Config, error) {
	root, err := ResolveRoot(options.Root, options.Home)
	if err != nil {
		return nil, err
	}
	path, err := root.Config()
	if err != nil {
		return nil, err
	}
	c := &Config{Capacity: Capacity{4, 2, 3, 2}, Shed: Shed{3, 3}}
	md, err := decode(path, c, false)
	if err != nil {
		return nil, err
	}
	c.Root = root
	if c.Version != 1 {
		return nil, fieldError(path, "version", "must be 1 (no migrations supported)")
	}
	ids := make([]ProjectID, len(c.ActiveProjects))
	for i, s := range c.ActiveProjects {
		ids[i], err = ParseProjectID(s)
		if err != nil {
			return nil, fieldError(path, fmt.Sprintf("active_projects[%d]", i), err.Error())
		}
	}
	if err := CheckProjectIDs(ids...); err != nil {
		return nil, fieldError(path, "active_projects", err.Error())
	}
	if len(ids) != 1 {
		return nil, fieldError(path, "active_projects", "M1 requires exactly one active project; multi-project operation is unsupported")
	}
	if !md.IsDefined("listen", "socket") {
		c.Listen.Socket, err = root.Socket()
	} else {
		c.Listen.Socket, err = resolvePath(c.Listen.Socket, options.Home, root.String())
	}
	if err != nil {
		return nil, fieldError(path, "listen.socket", err.Error())
	}
	if !beneath(root.String(), c.Listen.Socket) {
		return nil, fieldError(path, "listen.socket", "must stay beneath the Osmia root")
	}
	if filepath.Dir(c.Listen.Socket) != root.String() || !strings.HasSuffix(c.Listen.Socket, ".sock") {
		return nil, fieldError(path, "listen.socket", "must be a direct child of the root with a .sock suffix, separate from managed state")
	}
	if err := validateSocket(c.Listen.Socket); err != nil {
		return nil, fieldError(path, "listen.socket", err.Error())
	}
	for _, value := range []struct {
		field string
		n     int
	}{
		{"capacity.masons", c.Capacity.Masons}, {"capacity.reviewers", c.Capacity.Reviewers},
		{"capacity.committee", c.Capacity.Committee}, {"capacity.per_workstream", c.Capacity.PerWorkstream},
		{"shed.max_rounds", c.Shed.MaxRounds}, {"shed.max_bounces", c.Shed.MaxBounces},
	} {
		if value.n <= 0 {
			return nil, fieldError(path, value.field, "must be positive")
		}
	}
	if err := c.validateProfiles(path, md); err != nil {
		return nil, err
	}
	projectPath, err := root.ProjectConfig(ids[0])
	if err != nil {
		return nil, err
	}
	p := Project{BaseBranch: "main", Landing: "commit-per-unit", Capacity: ProjectCapacity{c.Capacity.PerWorkstream}}
	if _, err := decode(projectPath, &p, true); err != nil {
		return nil, err
	}
	p.ID = ids[0]
	if p.Version != 1 {
		return nil, fieldError(projectPath, "version", "must be 1 (no migrations supported)")
	}
	for _, value := range []struct{ field, s string }{{"upstream", p.Upstream}, {"fork", p.Fork}} {
		if !repositoryName.MatchString(value.s) || strings.HasSuffix(value.s, ".git") {
			return nil, fieldError(projectPath, value.field, "expected owner/repository without a URL or .git suffix")
		}
	}
	if strings.EqualFold(p.Upstream, p.Fork) {
		return nil, fieldError(projectPath, "fork", "must differ from upstream")
	}
	p.Clone, err = resolvePath(p.Clone, options.Home, filepath.Dir(projectPath))
	if err != nil {
		return nil, fieldError(projectPath, "clone", err.Error())
	}
	if beneath(p.Clone, root.String()) || beneath(root.String(), p.Clone) {
		return nil, fieldError(projectPath, "clone", "clone and Osmia root must be separate, non-nested directories")
	}
	if info, err := os.Stat(p.Clone); err == nil && !info.IsDir() {
		return nil, fieldError(projectPath, "clone", "must be a directory")
	}
	if !validBranch(p.BaseBranch) {
		return nil, fieldError(projectPath, "base_branch", "invalid Git branch name")
	}
	if !slices.Contains([]string{"commit-per-unit", "squash"}, p.Landing) {
		return nil, fieldError(projectPath, "landing", "expected commit-per-unit or squash")
	}
	if p.Capacity.PerWorkstream <= 0 {
		return nil, fieldError(projectPath, "capacity.per_workstream", "must be positive")
	}
	c.Project = p
	return c, nil
}

func decode(path string, dest any, project bool) (toml.MetaData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return toml.MetaData{}, fmt.Errorf("%s: %w", path, err)
	}
	md, err := toml.Decode(string(data), dest)
	if err != nil {
		return md, fmt.Errorf("%s: %w", path, err)
	}
	for _, key := range md.Keys() {
		if !knownKey(key, project) {
			return md, fieldError(path, key.String(), unsupportedKey(key))
		}
	}
	for _, key := range md.Undecoded() {
		return md, fieldError(path, key.String(), unsupportedKey(key))
	}
	return md, nil
}
func knownKey(key toml.Key, project bool) bool {
	path := key.String()
	if project {
		return slices.Contains([]string{"version", "name", "upstream", "fork", "clone", "base_branch", "landing", "capacity", "capacity.per_workstream"}, path)
	}
	if len(key) >= 2 && (key[0] == "profiles" || key[0] == "roles") {
		if len(key) == 2 {
			return true
		}
		if len(key) != 3 {
			return false
		}
		if key[0] == "profiles" {
			return slices.Contains([]string{"agent", "model", "effort", "fallback", "timeout", "max_turns"}, key[2])
		}
		return slices.Contains([]string{"profile", "sandbox", "image"}, key[2])
	}
	return slices.Contains([]string{"version", "active_projects", "listen", "listen.socket", "capacity", "capacity.masons", "capacity.reviewers", "capacity.committee", "capacity.per_workstream", "profiles", "roles", "shed", "shed.max_rounds", "shed.max_bounces"}, path)
}

func unsupportedKey(key toml.Key) string {
	switch key[0] {
	case "budget", "hearsay", "notify", "upstream_rebase", "hearsay_scope", "pause", "priority":
		return "unsupported in M1; requires a later milestone"
	}
	if key.String() == "listen.tailnet" || key.String() == "listen.web" {
		return "unsupported in M1; only a local Unix socket is supported"
	}
	return "unknown configuration key"
}
func (c *Config) validateProfiles(path string, md toml.MetaData) error {
	if len(c.Profiles) == 0 {
		return fieldError(path, "profiles", "at least one named profile is required")
	}
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		p := c.Profiles[name]
		prefix := "profiles." + name
		if !profileName.MatchString(name) {
			return fieldError(path, prefix, "expected lowercase profile name (letters, digits, _ or -; max 64)")
		}
		if !slices.Contains([]string{"claude", "codex", "opencode"}, p.Agent) {
			return fieldError(path, prefix+".agent", "expected claude, codex or opencode")
		}
		if strings.TrimSpace(p.Model) == "" {
			return fieldError(path, prefix+".model", "model is required")
		}
		if !md.IsDefined("profiles", name, "effort") {
			p.Effort = "medium"
		}
		if !slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, p.Effort) {
			return fieldError(path, prefix+".effort", "expected low, medium, high, xhigh or max")
		}
		if !md.IsDefined("profiles", name, "timeout") {
			p.Timeout = "45m"
		}
		timeout, err := time.ParseDuration(p.Timeout)
		if err != nil || timeout <= 0 {
			return fieldError(path, prefix+".timeout", "expected positive Go duration, for example 45m")
		}
		if p.MaxTurns < 0 || (p.MaxTurns > 0 && p.Agent != "claude") {
			return fieldError(path, prefix+".max_turns", "must be nonnegative; only claude enforces a nonzero limit")
		}
		if p.Fallback != "" {
			if _, ok := c.Profiles[p.Fallback]; !ok {
				return fieldError(path, prefix+".fallback", "unknown profile "+p.Fallback)
			}
		}
		c.Profiles[name] = p
	}
	// TODO: Remove this named-profile graph validation when busybees/core provides
	// cross-backend named fallback resolution and cycle validation.
	for _, name := range names {
		seen := map[string]bool{}
		for next := name; next != ""; next = c.Profiles[next].Fallback {
			if seen[next] {
				return fieldError(path, "profiles."+next+".fallback", "fallback cycle from "+name)
			}
			seen[next] = true
		}
	}
	if c.Roles == nil {
		c.Roles = map[string]Role{}
	}
	names = names[:0]
	for name := range c.Roles {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !slices.Contains(roleNames, name) {
			return fieldError(path, "roles."+name, "unknown role")
		}
	}
	for _, name := range roleNames {
		r := c.Roles[name]
		prefix := "roles." + name
		if !md.IsDefined("roles", name, "profile") {
			r.Profile = "default"
		}
		if _, ok := c.Profiles[r.Profile]; !ok {
			return fieldError(path, prefix+".profile", "unknown profile "+r.Profile)
		}
		if !md.IsDefined("roles", name, "sandbox") {
			r.Sandbox = "none"
		}
		if !slices.Contains([]string{"none", "claude", "container"}, r.Sandbox) {
			return fieldError(path, prefix+".sandbox", "expected none, claude or container")
		}
		if r.Sandbox == "container" && strings.TrimSpace(r.Image) == "" {
			return fieldError(path, prefix+".image", "container requires an explicit image")
		}
		if r.Sandbox != "container" && r.Image != "" {
			return fieldError(path, prefix+".image", "image requires container sandbox")
		}
		for next := r.Profile; next != ""; next = c.Profiles[next].Fallback {
			if r.Sandbox == "claude" && c.Profiles[next].Agent != "claude" {
				return fieldError(path, prefix+".sandbox", "claude sandbox requires claude in the entire fallback chain")
			}
		}
		c.Roles[name] = r
	}
	return nil
}

// Execution returns adapter inputs, not verified isolation or permission to run.
// profile may be a validated fallback selected by the service for this role.
func (c *Config) Execution(role, profile string) (coreadapter.Profile, coreadapter.ExecutionSettings, error) {
	r, ok := c.Roles[role]
	if !ok {
		return coreadapter.Profile{}, coreadapter.ExecutionSettings{}, fmt.Errorf("unknown role %q", role)
	}
	for next := r.Profile; next != ""; next = c.Profiles[next].Fallback {
		if next == profile {
			p := c.Profiles[next]
			timeout, err := time.ParseDuration(p.Timeout)
			return coreadapter.Profile{Name: next, Backend: p.Agent, Model: p.Model, Effort: p.Effort, Timeout: timeout, MaxTurns: p.MaxTurns}, coreadapter.ExecutionSettings{Mode: r.Sandbox, Image: r.Image}, err
		}
	}
	return coreadapter.Profile{}, coreadapter.ExecutionSettings{}, fmt.Errorf("profile %q is not in role %q's fallback chain", profile, role)
}
func validateSocket(path string) error {
	// 103 bytes fits Darwin's sockaddr_un and Linux's larger limit, with NUL.
	if len(path) > 103 {
		return fmt.Errorf("Unix socket path exceeds 103 bytes; choose a shorter root/path")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("existing path is not a Unix socket")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
func validBranch(s string) bool {
	if s == "" || s == "@" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, " ~^:?*[\\") || strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
