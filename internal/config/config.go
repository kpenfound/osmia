// Package config loads the declarative M1 configuration without creating state
// or starting execution. A successful load is not an execution capability grant.
package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

type Options struct{ Root, Home string }
type Config struct {
	Root           Root               `toml:"-" json:"-"`
	Version        int                `toml:"version" json:"version"`
	ActiveProjects []string           `toml:"active_projects" json:"active_projects"`
	Listen         Listen             `toml:"listen" json:"listen"`
	Capacity       Capacity           `toml:"capacity" json:"capacity"`
	Budget         Budget             `toml:"budget" json:"budget"`
	Profiles       map[string]Profile `toml:"profiles" json:"profiles"`
	Roles          map[string]Role    `toml:"roles" json:"roles"`
	Shed           Shed               `toml:"shed" json:"shed"`
	Mason          Mason              `toml:"mason" json:"mason"`
	Events         Events             `toml:"events" json:"events"`
	Project        Project            `toml:"-" json:"project"`
}
type Listen struct {
	Socket string `toml:"socket" json:"socket"`
}
type Capacity struct {
	Masons        int `toml:"masons" json:"masons"`
	Reviewers     int `toml:"reviewers" json:"reviewers"`
	Committee     int `toml:"committee" json:"committee"`
	PerWorkstream int `toml:"per_workstream" json:"per_workstream"`
}
type Budget struct {
	PerSession string `toml:"per_session" json:"per_session,omitempty"`
	PerUnit    string `toml:"per_unit" json:"per_unit,omitempty"`
	PerDay     string `toml:"per_day" json:"per_day,omitempty"`
}

var decimalUSD = regexp.MustCompile(`^(?:0|[1-9][0-9]*)(?:\.[0-9]+)?$`)

func (b Budget) SessionLimitUSD() float64 {
	v, _ := strconv.ParseFloat(b.PerSession, 64)
	return v
}

type Shed struct {
	MaxRounds  int `toml:"max_rounds" json:"max_rounds"`
	MaxBounces int `toml:"max_bounces" json:"max_bounces"`
}
type Mason struct {
	MaxCleanTurns int `toml:"max_clean_turns" json:"max_clean_turns"`
}
type Events struct {
	Window string `toml:"window" json:"window"`
}

// EventWindow is how long the service collects a workstream's events before
// delivering them to its chief of staff as one turn.
func (c *Config) EventWindow() time.Duration {
	d, _ := time.ParseDuration(c.Events.Window)
	return d
}

type Profile struct {
	Agent    string `toml:"agent" json:"agent"`
	Model    string `toml:"model" json:"model"`
	Effort   string `toml:"effort" json:"effort"`
	Fallback string `toml:"fallback" json:"fallback"`
	Timeout  string `toml:"timeout" json:"timeout"`
	MaxTurns int    `toml:"max_turns" json:"max_turns"`
}
type Role struct {
	Profile string `toml:"profile" json:"profile"`
	Sandbox string `toml:"sandbox" json:"sandbox"`
	Image   string `toml:"image" json:"image"`
}
type Project struct {
	ID         ProjectID       `toml:"-" json:"id"`
	Version    int             `toml:"version" json:"version"`
	Name       string          `toml:"name" json:"name"`
	Upstream   string          `toml:"upstream" json:"upstream"`
	Fork       string          `toml:"fork" json:"fork"`
	Clone      string          `toml:"clone" json:"clone"`
	BaseBranch string          `toml:"base_branch" json:"base_branch"`
	Landing    string          `toml:"landing" json:"landing"`
	Classifier string          `toml:"classifier" json:"classifier"`
	Capacity   ProjectCapacity `toml:"capacity" json:"capacity"`
}
type ProjectCapacity struct {
	PerWorkstream int `toml:"per_workstream" json:"per_workstream"`
}

var roleNames = []string{"architect", "chief_of_staff", "committee", "foreman", "librarian", "mason", "reviewer"}
var profileName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func fieldError(path, field, reason string) error {
	return fmt.Errorf("%s: %s: %s", path, field, reason)
}

// Load returns nil on every failure. The top-level file is required, and only
// the explicitly active project is read; archived directories are not activated.
func Load(options Options) (*Config, error) {
	root, err := ResolveRoot(options.Root, options.Home)
	if err != nil {
		return nil, err
	}
	path, err := root.Config()
	if err != nil {
		return nil, err
	}
	c := &Config{Capacity: Capacity{4, 2, 3, 2}, Shed: Shed{3, 3}, Mason: Mason{3}, Events: Events{"5s"}}
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
	if len(ids) > 1 {
		return nil, fieldError(path, "active_projects", "at most one active project is supported; multi-project operation is unsupported")
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
		{"mason.max_clean_turns", c.Mason.MaxCleanTurns},
	} {
		if value.n <= 0 {
			return nil, fieldError(path, value.field, "must be positive")
		}
	}
	if d, err := time.ParseDuration(c.Events.Window); err != nil || d <= 0 {
		return nil, fieldError(path, "events.window", "must be a positive Go duration")
	}
	for _, value := range []struct{ field, amount string }{{"per_session", c.Budget.PerSession}, {"per_unit", c.Budget.PerUnit}, {"per_day", c.Budget.PerDay}} {
		if !md.IsDefined("budget", value.field) {
			continue
		}
		n, err := strconv.ParseFloat(value.amount, 64)
		if !decimalUSD.MatchString(value.amount) || err != nil || n <= 0 || math.IsInf(n, 0) {
			return nil, fieldError(path, "budget."+value.field, "must be a positive decimal USD string")
		}
	}
	if err := c.validateProfiles(path, md); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return c, nil
	}
	p, err := loadProject(root, ids[0], options.Home, c.Capacity.PerWorkstream)
	if err != nil {
		return nil, err
	}
	c.Project = p
	if err := c.validateClassifier(); err != nil {
		return nil, err
	}
	return c, nil
}

// HasProject reports whether an active project is configured. Without one the
// Project field is zero and project-scoped operations are unavailable.
func (c *Config) HasProject() bool { return c.Project.ID != "" }

// WithProject returns a copy of c whose only active project is id, loaded and
// validated from its configuration file. The receiver is unchanged.
func (c *Config) WithProject(id ProjectID, home string) (*Config, error) {
	p, err := loadProject(c.Root, id, home, c.Capacity.PerWorkstream)
	if err != nil {
		return nil, err
	}
	out := *c
	out.ActiveProjects = []string{string(id)}
	out.Project = p
	if err := out.validateClassifier(); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Config) validateClassifier() error {
	if c.Project.Classifier == "" {
		return nil
	}
	if _, ok := c.Profiles[c.Project.Classifier]; !ok {
		path, _ := c.Root.ProjectConfig(c.Project.ID)
		return fieldError(path, "classifier", "unknown profile "+c.Project.Classifier)
	}
	if c.Roles["mason"].Sandbox == "claude" && c.Profiles[c.Project.Classifier].Agent != "claude" {
		path, _ := c.Root.ProjectConfig(c.Project.ID)
		return fieldError(path, "classifier", "mason claude sandbox requires a claude classifier profile")
	}
	return nil
}

// WithoutProject returns a copy of c with no active project.
func (c *Config) WithoutProject() *Config {
	out := *c
	out.ActiveProjects = []string{}
	out.Project = Project{}
	return &out
}

func loadProject(root Root, id ProjectID, home string, perWorkstream int) (Project, error) {
	projectPath, err := root.ProjectConfig(id)
	if err != nil {
		return Project{}, err
	}
	p := Project{BaseBranch: "main", Landing: "commit-per-unit", Capacity: ProjectCapacity{perWorkstream}}
	if _, err := decode(projectPath, &p, true); err != nil {
		return Project{}, err
	}
	p.ID = id
	if p.Version != 1 {
		return Project{}, fieldError(projectPath, "version", "must be 1 (no migrations supported)")
	}
	for _, value := range []struct{ field, s string }{{"upstream", p.Upstream}, {"fork", p.Fork}} {
		if !ValidRepository(value.s) {
			return Project{}, fieldError(projectPath, value.field, "expected owner/repository without a URL or .git suffix")
		}
	}
	if strings.EqualFold(p.Upstream, p.Fork) {
		return Project{}, fieldError(projectPath, "fork", "must differ from upstream")
	}
	p.Clone, err = resolvePath(p.Clone, home, filepath.Dir(projectPath))
	if err != nil {
		return Project{}, fieldError(projectPath, "clone", err.Error())
	}
	if root.Overlaps(p.Clone) {
		return Project{}, fieldError(projectPath, "clone", "clone and Osmia root must be separate, non-nested directories")
	}
	if info, err := os.Stat(p.Clone); err == nil && !info.IsDir() {
		return Project{}, fieldError(projectPath, "clone", "must be a directory")
	}
	if !ValidBranch(p.BaseBranch) {
		return Project{}, fieldError(projectPath, "base_branch", "invalid Git branch name")
	}
	if !slices.Contains([]string{"commit-per-unit", "squash"}, p.Landing) {
		return Project{}, fieldError(projectPath, "landing", "expected commit-per-unit or squash")
	}
	if p.Capacity.PerWorkstream <= 0 {
		return Project{}, fieldError(projectPath, "capacity.per_workstream", "must be positive")
	}
	return p, nil
}

// ValidRepository accepts owner/repository references without a URL or .git suffix.
func ValidRepository(s string) bool {
	return repositoryName.MatchString(s) && !strings.HasSuffix(s, ".git")
}

// ResolveClone resolves a clone path the way project configuration does, without
// requiring it to exist. Relative paths resolve against the process working
// directory, so callers pass absolute paths from a client.
func ResolveClone(clone, home string) (string, error) { return resolvePath(clone, home, "") }

// Overlaps reports whether path is the root, inside it, or contains it.
func (r Root) Overlaps(path string) bool {
	return beneath(path, r.directory) || beneath(r.directory, path)
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
		return slices.Contains([]string{"version", "name", "upstream", "fork", "clone", "base_branch", "landing", "classifier", "capacity", "capacity.per_workstream"}, path)
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
	return slices.Contains([]string{"version", "active_projects", "listen", "listen.socket", "capacity", "capacity.masons", "capacity.reviewers", "capacity.committee", "capacity.per_workstream", "budget", "budget.per_session", "budget.per_unit", "budget.per_day", "profiles", "roles", "shed", "shed.max_rounds", "shed.max_bounces", "mason", "mason.max_clean_turns", "events", "events.window"}, path)
}

func unsupportedKey(key toml.Key) string {
	switch key[0] {
	case "hearsay", "notify", "upstream_rebase", "hearsay_scope", "pause", "priority":
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

// NamedProfile returns the adapter profile of a configured profile, whichever
// role binds it. Runtime profile overrides may name any such profile.
func (c *Config) NamedProfile(name string) (coreadapter.Profile, error) {
	p, ok := c.Profiles[name]
	if !ok {
		return coreadapter.Profile{}, fmt.Errorf("unknown profile %q", name)
	}
	timeout, err := time.ParseDuration(p.Timeout)
	return coreadapter.Profile{Name: name, Backend: p.Agent, Model: p.Model, Effort: p.Effort, Timeout: timeout, MaxTurns: p.MaxTurns, CostLimitUSD: c.Budget.SessionLimitUSD()}, err
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
			return coreadapter.Profile{Name: next, Backend: p.Agent, Model: p.Model, Effort: p.Effort, Timeout: timeout, MaxTurns: p.MaxTurns, CostLimitUSD: c.Budget.SessionLimitUSD()}, coreadapter.ExecutionSettings{Mode: r.Sandbox, Image: r.Image}, err
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

// ValidBranch applies Git branch-name constraints.
func ValidBranch(s string) bool {
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
