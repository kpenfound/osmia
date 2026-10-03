// Package config loads the declarative service configuration without creating state
// or starting execution. A successful load is not an execution capability grant.
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// Options locates the configuration. File, when set, is the top-level file in
// place of the one ResolveRoot chooses; Root.Options pins both.
type Options struct{ Root, Home, File string }
type Config struct {
	Root           Root               `toml:"-" json:"-"`
	Version        int                `toml:"version" json:"version"`
	ActiveProjects []string           `toml:"active_projects" json:"active_projects"`
	Workspaces     string             `toml:"workspaces" json:"workspaces"`
	Listen         Listen             `toml:"listen" json:"listen"`
	Capacity       Capacity           `toml:"capacity" json:"capacity"`
	Budget         Budget             `toml:"budget" json:"budget"`
	Profiles       map[string]Profile `toml:"profiles" json:"profiles"`
	Roles          map[string]Role    `toml:"roles" json:"roles"`
	Shed           Shed               `toml:"shed" json:"shed"`
	Committee      Committee          `toml:"committee" json:"committee"`
	Mason          Mason              `toml:"mason" json:"mason"`
	Events         Events             `toml:"events" json:"events"`
	Hearsay        Hearsay            `toml:"hearsay" json:"hearsay"`
	Jev            Jev                `toml:"jev" json:"jev"`
	Notify         Notify             `toml:"notify" json:"notify"`
	// Project is the project this configuration is about: the only active
	// project of a loaded configuration, or the project For selected. It is
	// zero when no project, or more than one, is active.
	Project Project `toml:"-" json:"project"`
	// Projects holds every active project, in active_projects order.
	Projects []Project `toml:"-" json:"projects,omitempty"`
}
type Listen struct {
	Socket string `toml:"socket" json:"socket"`
	// Web is an optional loopback host:port that serves the same API as the
	// socket; empty disables it.
	Web string `toml:"web" json:"web,omitempty"`
	// Tailnet is an optional hostname under which the service joins the
	// owner's tailnet and serves the same API; empty disables it.
	Tailnet string `toml:"tailnet" json:"tailnet,omitempty"`
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

// Committee assigns shed debate perspectives and profiles to committee
// members. Member n takes element (n-1) modulo the list's length of each list;
// an empty Profiles leaves every member on the committee role's profile.
type Committee struct {
	Perspectives []string `toml:"perspectives" json:"perspectives"`
	Profiles     []string `toml:"profiles" json:"profiles"`
}

// CommitteePerspectives are the review perspectives a committee member may
// take in the shed.
var CommitteePerspectives = []string{"correctness", "integration", "scope"}

type Mason struct {
	MaxCleanTurns int `toml:"max_clean_turns" json:"max_clean_turns"`
}

// Notify configures owner notifications. Webhook is the absolute http or
// https URL each new inbox entry is posted to; empty sends nothing.
type Notify struct {
	Webhook string `toml:"webhook" json:"webhook,omitempty"`
}

// The workspaces values. WorkspacesAuto gives each new workstream Jujutsu
// workspaces when a supported jj is installed and Git worktrees otherwise;
// WorkspacesGit and WorkspacesJujutsu give it that backend. A workstream
// records WorkspacesGit or WorkspacesJujutsu, never WorkspacesAuto.
const (
	WorkspacesAuto    = "auto"
	WorkspacesGit     = "git"
	WorkspacesJujutsu = "jujutsu"
)

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
	// Dagger gives the mason's sbx sandbox the Dagger CLI and an engine on
	// the host; nil gives neither.
	Dagger *Dagger `toml:"dagger" json:"dagger,omitempty"`
}

// Dagger is the Dagger CLI release installed in a sandbox and the host engine
// it runs against.
type Dagger struct {
	Version string `toml:"version" json:"version"`
	Engine  string `toml:"engine" json:"engine"`
}

// Settings is d as execution settings carry it; nil for nil.
func (d *Dagger) Settings() *agent.Dagger {
	if d == nil {
		return nil
	}
	return &agent.Dagger{Engine: d.Engine, Version: d.Version}
}

type Project struct {
	ID              ProjectID         `toml:"-" json:"id"`
	Version         int               `toml:"version" json:"version"`
	Name            string            `toml:"name" json:"name"`
	Upstream        string            `toml:"upstream" json:"upstream"`
	Fork            string            `toml:"fork" json:"fork"`
	Clone           string            `toml:"clone" json:"clone"`
	BaseBranch      string            `toml:"base_branch" json:"base_branch"`
	Landing         string            `toml:"landing" json:"landing"`
	Classifier      string            `toml:"classifier" json:"classifier"`
	HearsayScope    string            `toml:"hearsay_scope" json:"hearsay_scope,omitempty"`
	HearsayEntities map[string]string `toml:"hearsay_entities" json:"hearsay_entities,omitempty"`
	// UpstreamRebase is the Go duration between a workstream's scheduled
	// drift rebases; zero disables them.
	UpstreamRebase string `toml:"upstream_rebase" json:"upstream_rebase"`
	// ChecksTimeout is the Go duration one run of a candidate's checks may
	// take.
	ChecksTimeout string          `toml:"checks_timeout" json:"checks_timeout"`
	Capacity      ProjectCapacity `toml:"capacity" json:"capacity"`
}

// DefaultChecksTimeout and MaxChecksTimeout bound one run of a candidate's
// checks.
const (
	DefaultChecksTimeout = "15m"
	MaxChecksTimeout     = 24 * time.Hour
)

// CheckTimeout is how long one run of a candidate's checks may take.
func (p Project) CheckTimeout() time.Duration {
	d, _ := time.ParseDuration(p.ChecksTimeout)
	return d
}

// RebaseInterval is how long after a workstream's latest drift or final
// rebase its next drift rebase is scheduled, or zero when scheduled drift
// rebases are disabled.
func (p Project) RebaseInterval() time.Duration {
	d, _ := time.ParseDuration(p.UpstreamRebase)
	return d
}

type ProjectCapacity struct {
	PerWorkstream int `toml:"per_workstream" json:"per_workstream"`
}

var roleNames = []string{"architect", "chief_of_staff", "committee", "foreman", "librarian", "mason", "reviewer"}
var profileName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// FieldError is a configuration file that failed to load: the file, the
// field at fault (empty when the whole file is at fault) and why. Its text
// never contains values read from the file.
type FieldError struct {
	Path, Field, Reason string
	Err                 error
}

func (e *FieldError) Error() string {
	if e.Field == "" {
		return e.Path + ": " + e.Reason
	}
	return e.Path + ": " + e.Field + ": " + e.Reason
}
func (e *FieldError) Unwrap() error { return e.Err }

func fieldError(path, field, reason string) error {
	return &FieldError{Path: path, Field: field, Reason: reason}
}

// Load returns nil on every failure. The top-level file is required, and only
// the explicitly active projects are read, each of which must load and
// validate; archived directories are not activated.
func Load(options Options) (*Config, error) {
	c, err := LoadTopLevel(options)
	if err != nil {
		return nil, err
	}
	for _, s := range c.ActiveProjects {
		id, err := ParseProjectID(s)
		if err != nil {
			return nil, err
		}
		p, err := loadProject(c.Root, id, options.Home, c.Capacity.PerWorkstream)
		if err != nil {
			return nil, err
		}
		if err := c.validateClassifier(p); err != nil {
			return nil, err
		}
		c.Projects = append(c.Projects, p)
	}
	if len(c.Projects) == 1 {
		c.Project = c.Projects[0]
	}
	return c, nil
}

// LoadTopLevel loads and validates the top-level file alone, as Load does,
// and returns nil on every failure. It reads no project file: Project and
// Projects stay zero whatever active_projects lists.
func LoadTopLevel(options Options) (*Config, error) {
	root, err := options.Resolve()
	if err != nil {
		return nil, err
	}
	path, err := root.Config()
	if err != nil {
		return nil, err
	}
	c := &Config{Capacity: Capacity{4, 2, 3, 2}, Shed: Shed{3, 3}, Committee: Committee{Perspectives: slices.Clone(CommitteePerspectives)}, Mason: Mason{3}, Events: Events{"5s"}, Workspaces: WorkspacesAuto, Jev: defaultJev()}
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
	if c.Listen.Web != "" {
		if err := validateWeb(c.Listen.Web); err != nil {
			return nil, fieldError(path, "listen.web", err.Error())
		}
	}
	if c.Listen.Tailnet != "" {
		if err := validateTailnet(c.Listen.Tailnet); err != nil {
			return nil, fieldError(path, "listen.tailnet", err.Error())
		}
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
	if !slices.Contains([]string{WorkspacesAuto, WorkspacesGit, WorkspacesJujutsu}, c.Workspaces) {
		return nil, fieldError(path, "workspaces", fmt.Sprintf("must be %q, %q or %q", WorkspacesAuto, WorkspacesGit, WorkspacesJujutsu))
	}
	if err := c.Hearsay.validate(path); err != nil {
		return nil, err
	}
	if err := c.Jev.validate(path); err != nil {
		return nil, err
	}
	if c.Notify.Webhook != "" {
		if err := validateWebhook(c.Notify.Webhook); err != nil {
			return nil, fieldError(path, "notify.webhook", err.Error())
		}
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
	if err := c.validateCommittee(path); err != nil {
		return nil, err
	}
	return c, nil
}

// validateCommittee checks the committee's perspectives and profiles against
// the known perspectives, the named profiles and the committee's sandbox.
func (c *Config) validateCommittee(path string) error {
	if len(c.Committee.Perspectives) == 0 {
		return fieldError(path, "committee.perspectives", "at least one perspective is required")
	}
	for i, name := range c.Committee.Perspectives {
		if !slices.Contains(CommitteePerspectives, name) {
			return fieldError(path, fmt.Sprintf("committee.perspectives[%d]", i), "expected "+strings.Join(CommitteePerspectives, ", "))
		}
	}
	for i, name := range c.Committee.Profiles {
		field := fmt.Sprintf("committee.profiles[%d]", i)
		if _, ok := c.Profiles[name]; !ok {
			return fieldError(path, field, "unknown profile "+name)
		}
		for next := name; next != ""; next = c.Profiles[next].Fallback {
			if c.Roles["committee"].Sandbox == "claude" && c.Profiles[next].Agent != "claude" {
				return fieldError(path, field, "claude sandbox requires claude in the entire fallback chain")
			}
		}
	}
	return nil
}

// HasProject reports whether the configuration is about one project: the only
// active project, or the one For selected. Without one the Project field is
// zero.
func (c *Config) HasProject() bool { return c.Project.ID != "" }

// ProjectIDs returns the IDs of the active projects, in active_projects order.
func (c *Config) ProjectIDs() []ProjectID {
	ids := make([]ProjectID, len(c.Projects))
	for i, p := range c.Projects {
		ids[i] = p.ID
	}
	return ids
}

// Active reports whether id is an active project.
func (c *Config) Active(id ProjectID) bool {
	return slices.ContainsFunc(c.Projects, func(p Project) bool { return p.ID == id })
}

// For returns the configuration about the active project id: c itself when
// its Project is already id, otherwise a copy whose Project is that project,
// or zero when id is not active. The receiver is unchanged.
func (c *Config) For(id ProjectID) *Config {
	if c.Project.ID == id {
		return c
	}
	out := *c
	out.Project = Project{}
	for _, p := range c.Projects {
		if p.ID == id {
			out.Project = p
		}
	}
	return &out
}

// WithProject returns a copy of c whose only active project is id, loaded and
// validated from its configuration file. The receiver is unchanged.
func (c *Config) WithProject(id ProjectID, home string) (*Config, error) {
	p, err := loadProject(c.Root, id, home, c.Capacity.PerWorkstream)
	if err != nil {
		return nil, err
	}
	if err := c.validateClassifier(p); err != nil {
		return nil, err
	}
	out := *c
	out.ActiveProjects = []string{string(id)}
	out.Project = p
	out.Projects = []Project{p}
	return &out, nil
}

// AddProject loads one project's configuration while preserving the loaded
// settings of every other project.
func (c *Config) AddProject(id ProjectID, home string) (*Config, error) {
	if c.Active(id) {
		return c, nil
	}
	loaded, err := c.WithProject(id, home)
	if err != nil {
		return nil, err
	}
	out := *c
	out.ActiveProjects = append(slices.Clone(c.ActiveProjects), string(id))
	out.Projects = append(slices.Clone(c.Projects), loaded.Project)
	out.Project = Project{}
	if len(out.Projects) == 1 {
		out.Project = out.Projects[0]
	}
	return &out, nil
}

// WithProjects returns a copy of c whose active projects are ids, in that
// order, each loaded and validated from its configuration file. The receiver
// is unchanged.
func (c *Config) WithProjects(ids []ProjectID, home string) (*Config, error) {
	out := *c
	out.ActiveProjects, out.Projects, out.Project = []string{}, nil, Project{}
	for _, id := range ids {
		p, err := loadProject(c.Root, id, home, c.Capacity.PerWorkstream)
		if err != nil {
			return nil, err
		}
		if err := c.validateClassifier(p); err != nil {
			return nil, err
		}
		out.ActiveProjects = append(out.ActiveProjects, string(id))
		out.Projects = append(out.Projects, p)
	}
	if len(out.Projects) == 1 {
		out.Project = out.Projects[0]
	}
	return &out, nil
}

func (c *Config) validateClassifier(p Project) error {
	if p.Classifier == "" {
		return nil
	}
	if _, ok := c.Profiles[p.Classifier]; !ok {
		path, _ := c.Root.ProjectConfig(p.ID)
		return fieldError(path, "classifier", "unknown profile "+p.Classifier)
	}
	if c.Roles["mason"].Sandbox == "claude" && c.Profiles[p.Classifier].Agent != "claude" {
		path, _ := c.Root.ProjectConfig(p.ID)
		return fieldError(path, "classifier", "mason claude sandbox requires a claude classifier profile")
	}
	return nil
}

// WithoutProject returns a copy of c with no active project.
func (c *Config) WithoutProject() *Config {
	out := *c
	out.ActiveProjects = []string{}
	out.Project = Project{}
	out.Projects = nil
	return &out
}

// WithoutProjectID returns a copy of c without the active project id; the
// other active projects stay, in order.
func (c *Config) WithoutProjectID(id ProjectID) *Config {
	out := *c
	out.ActiveProjects, out.Projects, out.Project = []string{}, nil, Project{}
	for _, p := range c.Projects {
		if p.ID != id {
			out.ActiveProjects = append(out.ActiveProjects, string(p.ID))
			out.Projects = append(out.Projects, p)
		}
	}
	if len(out.Projects) == 1 {
		out.Project = out.Projects[0]
	}
	return &out
}

func loadProject(root Root, id ProjectID, home string, perWorkstream int) (Project, error) {
	projectPath, err := root.ProjectConfig(id)
	if err != nil {
		return Project{}, err
	}
	p := Project{BaseBranch: "main", Landing: "commit-per-unit", UpstreamRebase: "6h", ChecksTimeout: DefaultChecksTimeout, Capacity: ProjectCapacity{perWorkstream}}
	if _, err := decode(projectPath, &p, true); err != nil {
		return Project{}, err
	}
	p.ID = id
	if err := p.validateHearsay(projectPath); err != nil {
		return Project{}, err
	}
	if p.Version != 1 {
		return Project{}, fieldError(projectPath, "version", "must be 1 (no migrations supported)")
	}
	for _, value := range []struct{ field, s string }{{"upstream", p.Upstream}, {"fork", p.Fork}} {
		if value.field == "fork" && value.s == "" {
			continue
		}
		if !ValidRepository(value.s) {
			return Project{}, fieldError(projectPath, value.field, "expected owner/repository without a URL or .git suffix")
		}
	}
	if strings.EqualFold(p.Upstream, p.Fork) {
		p.Fork = ""
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
	if d, err := time.ParseDuration(p.UpstreamRebase); err != nil || d < 0 || d > 0 && d < time.Minute {
		return Project{}, fieldError(projectPath, "upstream_rebase", "must be a Go duration of at least 1m, or 0 to disable scheduled drift rebases")
	}
	if d, err := time.ParseDuration(p.ChecksTimeout); err != nil || d < time.Minute || d > MaxChecksTimeout {
		return Project{}, fieldError(projectPath, "checks_timeout", "must be a Go duration from 1m to 24h")
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
		return toml.MetaData{}, &FieldError{Path: path, Reason: "cannot be read", Err: err}
	}
	md, err := toml.Decode(string(data), dest)
	if err != nil {
		return md, decodeError(path, err)
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

// typeMismatch matches the TOML decoder's message for a value of the wrong
// type, which carries the key and line but no parse position.
var typeMismatch = regexp.MustCompile(`^toml: (?:line ([0-9]+) )?\(last key ("(?:[^"\\]|\\.)*")\): incompatible types`)

// decodeError names the key and line of a file the TOML decoder rejected,
// leaving out the decoder's text, which may quote the file.
func decodeError(path string, err error) error {
	var parse toml.ParseError
	if errors.As(err, &parse) {
		return &FieldError{Path: path, Field: parse.LastKey, Reason: fmt.Sprintf("invalid TOML at line %d", parse.Position.Line), Err: err}
	}
	if m := typeMismatch.FindStringSubmatch(err.Error()); m != nil {
		key, _ := strconv.Unquote(m[2])
		reason := "value has the wrong type"
		if m[1] != "" {
			reason += " at line " + m[1]
		}
		return &FieldError{Path: path, Field: key, Reason: reason, Err: err}
	}
	return &FieldError{Path: path, Reason: "invalid TOML", Err: err}
}

func knownKey(key toml.Key, project bool) bool {
	path := key.String()
	if project {
		if len(key) == 2 && key[0] == "hearsay_entities" {
			return true
		}
		return slices.Contains([]string{"hearsay_scope", "hearsay_entities", "version", "name", "upstream", "fork", "clone", "base_branch", "landing", "classifier", "upstream_rebase", "checks_timeout", "capacity", "capacity.per_workstream"}, path)
	}
	if key[0] == "hearsay" {
		if len(key) == 1 {
			return true
		}
		if len(key) == 2 {
			return slices.Contains([]string{"url", "principal", "token_env", "agents"}, key[1])
		}
		return key[1] == "agents" && (len(key) == 3 || len(key) == 4 && (key[3] == "id" || key[3] == "token_env"))
	}
	if len(key) >= 2 && (key[0] == "profiles" || key[0] == "roles") {
		if len(key) == 2 {
			return true
		}
		if key[0] == "roles" && len(key) == 4 && key[2] == "dagger" {
			return key[3] == "version" || key[3] == "engine"
		}
		if len(key) != 3 {
			return false
		}
		if key[0] == "profiles" {
			return slices.Contains([]string{"agent", "model", "effort", "fallback", "timeout", "max_turns"}, key[2])
		}
		return slices.Contains([]string{"profile", "sandbox", "image", "dagger"}, key[2])
	}
	return slices.Contains([]string{"version", "active_projects", "workspaces", "listen", "listen.socket", "listen.web", "listen.tailnet", "capacity", "capacity.masons", "capacity.reviewers", "capacity.committee", "capacity.per_workstream", "budget", "budget.per_session", "budget.per_unit", "budget.per_day", "profiles", "roles", "shed", "shed.max_rounds", "shed.max_bounces", "committee", "committee.perspectives", "committee.profiles", "mason", "mason.max_clean_turns", "events", "events.window", "notify", "notify.webhook", "jev", "jev.enabled", "jev.url", "jev.model", "jev.api_key_env", "jev.timeout"}, path)
}

func unsupportedKey(key toml.Key) string {
	switch key[0] {
	case "pause", "priority":
		return "runtime overrides belong in runtime.json; use osmia pause or osmia priority"
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
		if !slices.Contains([]string{"none", "claude", "container", "sbx"}, r.Sandbox) {
			return fieldError(path, prefix+".sandbox", "expected none, claude, container or sbx")
		}
		if r.Sandbox == "container" && strings.TrimSpace(r.Image) == "" {
			return fieldError(path, prefix+".image", "container requires an explicit image")
		}
		if r.Sandbox != "container" && r.Sandbox != "sbx" && r.Image != "" {
			return fieldError(path, prefix+".image", "image requires container or sbx sandbox")
		}
		if r.Dagger != nil {
			if name != "mason" {
				return fieldError(path, prefix+".dagger", "only the mason runs Dagger")
			}
			if r.Sandbox != "sbx" {
				return fieldError(path, prefix+".dagger", "dagger requires the sbx sandbox")
			}
			if err := agent.CheckDaggerVersion(r.Dagger.Version); err != nil {
				return fieldError(path, prefix+".dagger.version", err.Error())
			}
			if err := agent.CheckDaggerEngine(r.Dagger.Engine); err != nil {
				return fieldError(path, prefix+".dagger.engine", err.Error())
			}
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
			return coreadapter.Profile{Name: next, Backend: p.Agent, Model: p.Model, Effort: p.Effort, Timeout: timeout, MaxTurns: p.MaxTurns, CostLimitUSD: c.Budget.SessionLimitUSD()}, coreadapter.ExecutionSettings{Mode: r.Sandbox, Image: r.Image, Dagger: r.Dagger.Settings()}, err
		}
	}
	return coreadapter.Profile{}, coreadapter.ExecutionSettings{}, fmt.Errorf("profile %q is not in role %q's fallback chain", profile, role)
}
func validateSocket(path string) error {
	// 103 bytes fits Darwin's sockaddr_un and Linux's larger limit, with NUL.
	if len(path) > 103 {
		return fmt.Errorf("socket path exceeds the 103-byte Unix socket limit; choose a shorter root/path")
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

// validateWeb accepts host:port where the host is localhost or a loopback
// IP address and the port is a number from 0 to 65535; 0 binds a free port.
func validateWeb(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("expected host:port, such as 127.0.0.1:8080")
	}
	if host == "" {
		return fmt.Errorf("host is required; use localhost, 127.0.0.1 or [::1]")
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || strconv.FormatUint(n, 10) != port {
		return fmt.Errorf("port must be a number from 0 to 65535")
	}
	if LoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("host must be localhost or a loopback address; other interfaces are not supported")
}

// validateTailnet accepts a single DNS label: 1 to 63 lowercase letters,
// digits and hyphens that neither starts nor ends with a hyphen.
func validateTailnet(hostname string) error {
	if len(hostname) > 63 {
		return fmt.Errorf("hostname must be at most 63 characters")
	}
	for _, r := range hostname {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return fmt.Errorf("hostname must be one label of lowercase letters, digits and hyphens, such as osmia")
		}
	}
	if strings.HasPrefix(hostname, "-") || strings.HasSuffix(hostname, "-") {
		return fmt.Errorf("hostname must not start or end with a hyphen")
	}
	return nil
}

// validateWebhook accepts an absolute http or https URL with a host. The
// reason never quotes the URL, which may carry a secret.
func validateWebhook(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("must be an absolute http or https URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	return nil
}

// LoopbackHost reports whether host, without a port or brackets, is
// localhost or a loopback IP address.
func LoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// PushRepository names the repository receiving feature branches.
func (p Project) PushRepository() string {
	if p.Fork != "" {
		return p.Fork
	}
	return p.Upstream
}
