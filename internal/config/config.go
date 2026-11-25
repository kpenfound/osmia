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
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/skills"
)

// Options locates the configuration. File, when set, is the top-level file in
// place of the one ResolveRoot chooses; Root.Options pins both.
type Options struct{ Root, Home, File string }
type Config struct {
	Root           Root               `toml:"-" json:"-"`
	Version        int                `toml:"version" json:"version"`
	ActiveProjects []string           `toml:"-" json:"active_projects"`
	Workspaces     string             `toml:"workspaces" json:"workspaces"`
	Listen         Listen             `toml:"listen" json:"listen"`
	Capacity       Capacity           `toml:"capacity" json:"capacity"`
	Budget         Budget             `toml:"budget" json:"budget"`
	Dagger         DaggerConfig       `toml:"dagger" json:"dagger"`
	Profiles       map[string]Profile `toml:"profiles" json:"profiles"`
	Roles          map[string]Role    `toml:"roles" json:"roles"`
	Shed           Shed               `toml:"shed" json:"shed"`
	Committee      Committee          `toml:"committee" json:"committee"`
	Mason          Mason              `toml:"mason" json:"mason"`
	Loop           Loop               `toml:"loop" json:"loop"`
	Events         Events             `toml:"events" json:"events"`
	Hearsay        Hearsay            `toml:"hearsay" json:"hearsay"`
	Jev            Jev                `toml:"jev" json:"jev"`
	Transcribe     Transcribe         `toml:"transcribe" json:"transcribe"`
	Telemetry      Telemetry          `toml:"telemetry" json:"telemetry"`
	Notify         Notify             `toml:"notify" json:"notify"`
	ErrorReports   ErrorReports       `toml:"error_reports" json:"error_reports"`
	Skills         Skills             `toml:"skills" json:"skills"`
	// ProjectCreation is the [projects] table. Its JSON name differs because
	// Projects lists the active projects.
	ProjectCreation ProjectCreation `toml:"projects" json:"project_creation"`
	// Project is the project this configuration is about: the only active
	// project of a loaded configuration, or the project For selected. It is
	// zero when no project, or more than one, is active.
	Project Project `toml:"-" json:"project"`
	// Projects holds every active project, in registry order.
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
	// TailnetHTTPS serves HTTPS on the tailnet in addition to HTTP. Defaults to true.
	TailnetHTTPS bool `toml:"tailnet_https" json:"tailnet_https"`
}
type Capacity struct {
	Checks        int `toml:"checks" json:"checks"`
	Chief         int `toml:"chief" json:"chief"`
	Masons        int `toml:"masons" json:"masons"`
	Reviewers     int `toml:"reviewers" json:"reviewers"`
	Committee     int `toml:"committee" json:"committee"`
	PerWorkstream int `toml:"per_workstream" json:"per_workstream"`
	// LocalSlots bounds the model sessions that run on the server's own
	// machine, the built-in local client, whatever their role. Nil adds no cap
	// beyond the role limits above; 0 runs no session on the server's machine.
	LocalSlots *int `toml:"local_slots" json:"local_slots,omitempty"`
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

// Loop configures the loop guard and the stall watchdog. MaxSessions is how
// many agent sessions a workstream may run without progress before the loop
// guard pauses it; zero turns the guard off. StallAfter is a Go duration: how
// long a unit or workstream may go without progress, with nothing legitimate
// explaining the quiet, before the stall watchdog raises a stalled exit to the
// transition router; "0" turns the watchdog off. It is also how long a landing,
// rebase or publication that keeps failing is retried before it is ended, so
// "0" leaves that window at DefaultStallAfter.
type Loop struct {
	MaxSessions int    `toml:"max_sessions" json:"max_sessions"`
	StallAfter  string `toml:"stall_after" json:"stall_after"`
}

// DefaultStallAfter is the default of loop.stall_after ("45m"), and the window
// a failing operation is retried for when the watchdog is off.
const DefaultStallAfter = 45 * time.Minute

// StallWindow is how long a unit or workstream may go without progress before
// the stall watchdog raises a stalled exit; zero means the watchdog is off.
func (c *Config) StallWindow() time.Duration {
	d, _ := time.ParseDuration(c.Loop.StallAfter)
	return d
}

// GiveUpWindow is how long after its first failure an operation that keeps
// failing is retried before it is ended: loop.stall_after, or DefaultStallAfter
// when the stall watchdog is off.
func (c *Config) GiveUpWindow() time.Duration {
	if d := c.StallWindow(); d > 0 {
		return d
	}
	return DefaultStallAfter
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

// Skills configures the cache of the skills roles reference. Refresh is how
// stale a clone may get before a turn that needs it pulls it: "never",
// "always" or a duration.
type Skills struct {
	Refresh string `toml:"refresh" json:"refresh"`
}

type Events struct {
	Window         string `toml:"window" json:"window"`
	ProgressWindow string `toml:"progress_window" json:"progress_window"`
}

// EventWindow is how long the service collects a workstream's events before
// delivering them to its chief of staff as one turn.
func (c *Config) EventWindow() time.Duration {
	d, _ := time.ParseDuration(c.Events.Window)
	return d
}

const defaultProgressWindow = 15 * time.Minute

// EventProgressWindow is how long an event of routine progress waits for
// others before the service delivers it to the chief of staff.
func (c *Config) EventProgressWindow() time.Duration {
	d, _ := time.ParseDuration(c.Events.ProgressWindow)
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
	// EventsProfile is the profile of the chief of staff's turns that
	// deliver service events; empty uses Profile. Turns answering the owner
	// use Profile.
	EventsProfile string `toml:"events_profile" json:"events_profile,omitempty"`
	Sandbox       string `toml:"sandbox" json:"sandbox"`
	Image         string `toml:"image" json:"image"`
	// CPUs and Memory size the role's sbx sandboxes: a positive number of
	// CPUs and a memory limit such as 512m or 4g. Zero and empty leave the
	// size to sbx. They apply only with the sbx sandbox.
	CPUs   int    `toml:"cpus" json:"cpus,omitempty"`
	Memory string `toml:"memory" json:"memory,omitempty"`
	// Skills are git references (<url>[@<ref>][#<sub/dir>]) of skills given
	// to the role's Claude and Codex turns.
	Skills []string `toml:"skills" json:"skills,omitempty"`
}

func (c *Config) masonDagger(sandbox string) (*agent.Dagger, string) {
	if sandbox != "sbx" {
		return nil, ""
	}
	engine := c.Dagger.Resolve()
	switch {
	case engine.Mode == DaggerEngineCloud:
		return nil, engine.CloudComputeTokenEnv
	case engine.Address != "":
		return &agent.Dagger{Engine: engine.Address, Version: c.Dagger.Version}, ""
	}
	return nil, ""
}

// RoleExecution is how turns of role run: in the role's sandbox and image
// with the role's skills and, for a mason in sbx, Dagger on the service's
// engine.
func (c *Config) RoleExecution(role string) coreadapter.ExecutionSettings {
	r := c.Roles[role]
	execution := coreadapter.ExecutionSettings{Mode: r.Sandbox, Image: r.Image, CPUs: r.CPUs, Memory: r.Memory, Skills: slices.Clone(r.Skills)}
	if role == "mason" {
		execution.Dagger, execution.DaggerCloudTokenEnv = c.masonDagger(r.Sandbox)
	}
	return execution
}

func (c *Config) validateMasonDagger(path string) error {
	engine := c.Dagger.Resolve()
	switch engine.Mode {
	case DaggerEngineCloud:
		return nil
	case DaggerEngineAuto:
		return fieldError(path, "dagger.engine", "must be managed, cloud or an engine address: the mason's sbx sandbox runs Dagger on the service's engine")
	}
	if c.Dagger.Version == "" {
		return fieldError(path, "dagger.version", "is required: the mason's sbx sandbox installs this Dagger CLI release to run on the service's engine")
	}
	return nil
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
	Delivery        string            `toml:"delivery" json:"delivery"`
	HearsayScope    string            `toml:"hearsay_scope" json:"hearsay_scope,omitempty"`
	HearsayEntities map[string]string `toml:"hearsay_entities" json:"hearsay_entities,omitempty"`
	// HearsayDeclareRepo posts the project to Hearsay with its GitHub repo
	// declared, which makes its records visible to the repository's readers.
	// Off, the project is posted without repos.
	HearsayDeclareRepo bool `toml:"hearsay_declare_repo" json:"hearsay_declare_repo,omitempty"`
	// FlakeFixes turns flake-fix workstreams on for the project: with it on,
	// a flaky check is recorded and the chief of staff may open a workstream
	// to fix its cause. Off, a flaky check is only counted as a pass.
	FlakeFixes bool `toml:"flake_fixes" json:"flake_fixes,omitempty"`
	// UpstreamRebase is the Go duration between a workstream's scheduled
	// drift rebases; zero disables them.
	UpstreamRebase string `toml:"upstream_rebase" json:"upstream_rebase"`
	// PullRequestPoll bounds attempts to observe pull requests.
	PullRequestPoll string `toml:"pull_request_poll" json:"pull_request_poll"`
	// ChecksTimeout is the Go duration one run of a candidate's checks may
	// take.
	ChecksTimeout string          `toml:"checks_timeout" json:"checks_timeout"`
	Capacity      ProjectCapacity `toml:"capacity" json:"capacity"`
}

// DefaultChecksTimeout and MaxChecksTimeout bound one run of a candidate's
// checks. DefaultPullRequestPoll bounds pull request observation attempts.
const (
	DefaultChecksTimeout   = "15m"
	DefaultPullRequestPoll = "10m"
	MaxChecksTimeout       = 24 * time.Hour
)

// CheckTimeout is how long one run of a candidate's checks may take.
func (p Project) CheckTimeout() time.Duration {
	d, _ := time.ParseDuration(p.ChecksTimeout)
	return d
}

// PullRequestPollInterval is the minimum time between pull request checks.
func (p Project) PullRequestPollInterval() time.Duration {
	d, _ := time.ParseDuration(p.PullRequestPoll)
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
	GitOperations int `toml:"git_operations" json:"git_operations"`
}

// DefaultGitOperations bounds one project's repository and GitHub operations.
const DefaultGitOperations = 4

var roleNames = []string{"architect", "chief_of_staff", "committee", "foreman", "mason", "reviewer"}
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

// LoadTopLevel loads and validates the top-level file. It returns nil on every
// failure and reads no project settings: Project and Projects are zero.
func LoadTopLevel(options Options) (*Config, error) {
	root, err := options.Resolve()
	if err != nil {
		return nil, err
	}
	path, err := root.Config()
	if err != nil {
		return nil, err
	}
	c := &Config{Listen: Listen{TailnetHTTPS: true}, Capacity: Capacity{Masons: 4, Reviewers: 2, Committee: 3, PerWorkstream: 2, Checks: 2, Chief: 3}, Shed: Shed{3, 3}, Committee: Committee{Perspectives: slices.Clone(CommitteePerspectives)}, Mason: Mason{3}, Loop: Loop{MaxSessions: 30, StallAfter: "45m"}, Events: Events{Window: "5s"}, Skills: Skills{skills.DefaultRefresh}, Workspaces: WorkspacesAuto, Jev: defaultJev(), Transcribe: defaultTranscribe(), Telemetry: defaultTelemetry(), ErrorReports: defaultErrorReports()}
	c.Dagger.Engine = string(DaggerEngineAuto)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &FieldError{Path: path, Reason: "cannot be read", Err: err}
	}
	md, err := decodeBytes(path, data, c, false)
	if err != nil {
		return nil, err
	}
	c.Root = root
	if c.Version != 1 {
		return nil, fieldError(path, "version", "must be 1 (no migrations supported)")
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
		{"capacity.checks", c.Capacity.Checks}, {"capacity.chief", c.Capacity.Chief}, {"capacity.masons", c.Capacity.Masons}, {"capacity.reviewers", c.Capacity.Reviewers},
		{"capacity.committee", c.Capacity.Committee}, {"capacity.per_workstream", c.Capacity.PerWorkstream},
		{"shed.max_rounds", c.Shed.MaxRounds}, {"shed.max_bounces", c.Shed.MaxBounces},
		{"mason.max_clean_turns", c.Mason.MaxCleanTurns},
	} {
		if value.n <= 0 {
			return nil, fieldError(path, value.field, "must be positive")
		}
	}
	if c.Capacity.LocalSlots != nil && *c.Capacity.LocalSlots < 0 {
		return nil, fieldError(path, "capacity.local_slots", "must not be negative")
	}
	if c.Loop.MaxSessions < 0 {
		return nil, fieldError(path, "loop.max_sessions", "must not be negative")
	}
	if d, err := time.ParseDuration(c.Loop.StallAfter); err != nil || d < 0 {
		return nil, fieldError(path, "loop.stall_after", `must be a Go duration that is not negative; "0" turns the stall watchdog off`)
	}
	if d, err := time.ParseDuration(c.Events.Window); err != nil || d <= 0 {
		return nil, fieldError(path, "events.window", "must be a positive Go duration")
	}
	if c.Events.ProgressWindow == "" {
		c.Events.ProgressWindow = max(defaultProgressWindow, c.EventWindow()).String()
	}
	if d, err := time.ParseDuration(c.Events.ProgressWindow); err != nil || d < c.EventWindow() {
		return nil, fieldError(path, "events.progress_window", "must be a Go duration no shorter than events.window")
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
	if err := c.Transcribe.validate(path); err != nil {
		return nil, err
	}
	if err := c.Telemetry.validate(path); err != nil {
		return nil, err
	}
	if err := c.ErrorReports.validate(path); err != nil {
		return nil, err
	}
	if err := c.ProjectCreation.validate(path, root, options.Home); err != nil {
		return nil, err
	}
	if _, _, err := skills.ParseRefresh(c.Skills.Refresh); err != nil {
		return nil, fieldError(path, "skills.refresh", err.Error())
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
	if err := c.Dagger.validate(path, md.IsDefined("dagger", "version")); err != nil {
		return nil, err
	}
	if err := c.validateProfiles(path, md); err != nil {
		return nil, err
	}
	if err := c.validateCommittee(path); err != nil {
		return nil, err
	}
	return c, nil
}

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

// ProjectIDs returns the IDs of the active projects, in registry order.
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

// WithLoadedProjects returns a copy of c whose active projects are ps, already
// loaded, in that order. It reads no file. The receiver is unchanged.
func (c *Config) WithLoadedProjects(ps []Project) *Config {
	out := *c
	out.ActiveProjects, out.Projects, out.Project = []string{}, slices.Clone(ps), Project{}
	for _, p := range ps {
		out.ActiveProjects = append(out.ActiveProjects, string(p.ID))
	}
	if len(out.Projects) == 1 {
		out.Project = out.Projects[0]
	}
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

func loadProjectBytes(root Root, id ProjectID, home string, perWorkstream int, data []byte) (Project, toml.MetaData, error) {
	projectPath, err := root.ProjectConfig(id)
	if err != nil {
		return Project{}, toml.MetaData{}, err
	}
	p := Project{BaseBranch: "main", Landing: "commit-per-unit", Delivery: DeliveryPullRequest, UpstreamRebase: "6h", PullRequestPoll: DefaultPullRequestPoll, ChecksTimeout: DefaultChecksTimeout, Capacity: ProjectCapacity{PerWorkstream: perWorkstream, GitOperations: DefaultGitOperations}}
	md, err := decodeBytes(projectPath, data, &p, true)
	if err != nil {
		return Project{}, md, err
	}
	if err := p.validate(root, id, home, projectPath); err != nil {
		return Project{}, md, err
	}
	return p, md, nil
}

func (p *Project) validate(root Root, id ProjectID, home, projectPath string) error {
	var err error
	p.ID = id
	if err := p.validateHearsay(projectPath); err != nil {
		return err
	}
	if p.Version != 1 {
		return fieldError(projectPath, "version", "must be 1 (no migrations supported)")
	}
	for _, value := range []struct{ field, s string }{{"upstream", p.Upstream}, {"fork", p.Fork}} {
		if value.field == "fork" && value.s == "" {
			continue
		}
		if !ValidRepository(value.s) {
			return fieldError(projectPath, value.field, "expected owner/repository without a URL or .git suffix")
		}
	}
	if strings.EqualFold(p.Upstream, p.Fork) {
		p.Fork = ""
	}
	p.Clone, err = resolvePath(p.Clone, home, filepath.Dir(projectPath))
	if err != nil {
		return fieldError(projectPath, "clone", err.Error())
	}
	if root.Overlaps(p.Clone) {
		return fieldError(projectPath, "clone", "clone and Osmia root must be separate, non-nested directories")
	}
	if info, err := os.Stat(p.Clone); err == nil && !info.IsDir() {
		return fieldError(projectPath, "clone", "must be a directory")
	}
	if !ValidBranch(p.BaseBranch) {
		return fieldError(projectPath, "base_branch", "invalid Git branch name")
	}
	if !slices.Contains([]string{"commit-per-unit", "squash"}, p.Landing) {
		return fieldError(projectPath, "landing", "expected commit-per-unit or squash")
	}
	if !slices.Contains([]string{DeliveryPullRequest, DeliveryJev}, p.Delivery) {
		return fieldError(projectPath, "delivery", "expected "+DeliveryPullRequest+" or "+DeliveryJev)
	}
	if p.Delivery == DeliveryJev && !strings.EqualFold(p.PushRepository(), p.Upstream) {
		return fieldError(projectPath, "delivery", DeliveryJev+" needs the project to push to its upstream: remove fork, or use "+DeliveryPullRequest)
	}
	if d, err := time.ParseDuration(p.UpstreamRebase); err != nil || d < 0 || d > 0 && d < time.Minute {
		return fieldError(projectPath, "upstream_rebase", "must be a Go duration of at least 1m, or 0 to disable scheduled drift rebases")
	}
	if d, err := time.ParseDuration(p.PullRequestPoll); err != nil || d < 5*time.Minute {
		return fieldError(projectPath, "pull_request_poll", "must be a Go duration of at least 5m")
	}
	if d, err := time.ParseDuration(p.ChecksTimeout); err != nil || d < time.Minute || d > MaxChecksTimeout {
		return fieldError(projectPath, "checks_timeout", "must be a Go duration from 1m to 24h")
	}
	if p.Capacity.PerWorkstream <= 0 {
		return fieldError(projectPath, "capacity.per_workstream", "must be positive")
	}
	if p.Capacity.GitOperations <= 0 {
		return fieldError(projectPath, "capacity.git_operations", "must be positive")
	}
	return nil
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

func decodeBytes(path string, data []byte, dest any, project bool) (toml.MetaData, error) {
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

var typeMismatch = regexp.MustCompile(`^toml: (?:line ([0-9]+) )?\(last key ("(?:[^"\\]|\\.)*")\): incompatible types`)

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
		return slices.Contains([]string{"hearsay_scope", "hearsay_entities", "hearsay_declare_repo", "flake_fixes", "version", "name", "upstream", "fork", "clone", "base_branch", "landing", "delivery", "upstream_rebase", "pull_request_poll", "checks_timeout", "capacity", "capacity.per_workstream", "capacity.git_operations"}, path)
	}
	if key[0] == "dagger" {
		return len(key) == 1 || len(key) == 2 && slices.Contains([]string{"version", "engine", "cloud_compute_token_env", "cloud_api_token_env", "check_fallback", "organization"}, key[1])
	}
	if key[0] == "hearsay" {
		if len(key) == 1 {
			return true
		}
		if len(key) == 2 {
			return slices.Contains([]string{"url", "principal", "token_env", "agents", "publish"}, key[1])
		}
		if key[1] == "publish" {
			return len(key) == 3 && slices.Contains([]string{"work_source", "owner_source", "hooks_url", "agent", "token_env"}, key[2])
		}
		return key[1] == "agents" && (len(key) == 3 || len(key) == 4 && (key[3] == "id" || key[3] == "token_env"))
	}
	if len(key) >= 2 && (key[0] == "profiles" || key[0] == "roles") {
		if key[0] == "roles" && !slices.Contains(roleNames, key[1]) {
			return false
		}
		if len(key) == 2 {
			return true
		}
		if len(key) != 3 {
			return false
		}
		if key[0] == "profiles" {
			return slices.Contains([]string{"agent", "model", "effort", "fallback", "timeout", "max_turns"}, key[2])
		}
		return slices.Contains([]string{"profile", "events_profile", "sandbox", "image", "cpus", "memory", "skills"}, key[2])
	}
	return slices.Contains([]string{"version", "workspaces", "listen", "listen.socket", "listen.web", "listen.tailnet", "listen.tailnet_https", "capacity", "capacity.checks", "capacity.chief", "capacity.masons", "capacity.reviewers", "capacity.committee", "capacity.per_workstream", "capacity.local_slots", "budget", "budget.per_session", "budget.per_unit", "budget.per_day", "profiles", "roles", "shed", "shed.max_rounds", "shed.max_bounces", "committee", "committee.perspectives", "committee.profiles", "mason", "mason.max_clean_turns", "loop", "loop.max_sessions", "loop.stall_after", "events", "events.window", "events.progress_window", "notify", "notify.webhook", "error_reports", "error_reports.enabled", "error_reports.repository", "error_reports.labels", "error_reports.max_issues_per_hour", "error_reports.max_issues_per_day", "error_reports.max_comments_per_day", "error_reports.sources", "skills", "skills.refresh", "jev", "jev.url", "jev.model", "jev.api_key_env", "jev.timeout", "jev.check_threshold", "jev.decision_threshold", "jev.owner_threshold", "jev.check_fallback_picks", "transcribe", "transcribe.url", "transcribe.model", "transcribe.api_key_env", "telemetry", "telemetry.endpoint", "telemetry.protocol", "telemetry.insecure", "telemetry.headers_env", "projects", "projects.repo_owner", "projects.clone_dir"}, path)
}

func unsupportedKey(key toml.Key) string {
	switch key[0] {
	case "pause", "priority":
		return "runtime overrides belong in runtime.json; use osmia pause or osmia priority"
	}
	if key.String() == "dagger.cloud_token_env" {
		return "is not a key: set cloud_compute_token_env to the token that runs checks and masons on the Cloud engine, and cloud_api_token_env to the API token that reruns Cloud checks and reads Cloud traces"
	}
	return "unknown configuration key"
}
func (c *Config) validateFallbackCycles(path string, names []string) error {
	for _, name := range names {
		seen := map[string]bool{}
		for next := name; next != ""; next = c.Profiles[next].Fallback {
			if seen[next] {
				return fieldError(path, "profiles."+next+".fallback", "fallback cycle from "+name)
			}
			seen[next] = true
		}
	}
	return nil
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
	if err := c.validateFallbackCycles(path, names); err != nil {
		return err
	}
	if c.Roles == nil {
		c.Roles = map[string]Role{}
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
		if md.IsDefined("roles", name, "cpus") {
			if r.Sandbox != "sbx" {
				return fieldError(path, prefix+".cpus", "cpus requires the sbx sandbox")
			}
			if r.CPUs <= 0 {
				return fieldError(path, prefix+".cpus", "must be a positive number of CPUs")
			}
		}
		if md.IsDefined("roles", name, "memory") {
			if r.Sandbox != "sbx" {
				return fieldError(path, prefix+".memory", "memory requires the sbx sandbox")
			}
			if r.Memory == "" || agent.CheckSandboxMemory(r.Memory) != nil {
				return fieldError(path, prefix+".memory", "must be a positive integer and the unit m or g, such as 512m or 4g")
			}
		}
		if name == "mason" && r.Sandbox == "sbx" {
			if err := c.validateMasonDagger(path); err != nil {
				return err
			}
		}
		names := map[string]bool{}
		for i, ref := range r.Skills {
			field := fmt.Sprintf("%s.skills[%d]", prefix, i)
			spec, err := skills.Parse(ref)
			if err != nil {
				return fieldError(path, field, err.Error())
			}
			if names[spec.Name] {
				return fieldError(path, field, "another skill of the role has the name "+spec.Name)
			}
			names[spec.Name] = true
		}
		if r.EventsProfile != "" {
			if name != "chief_of_staff" {
				return fieldError(path, prefix+".events_profile", "only the chief of staff takes event turns")
			}
			if _, ok := c.Profiles[r.EventsProfile]; !ok {
				return fieldError(path, prefix+".events_profile", "unknown profile "+r.EventsProfile)
			}
		}
		for _, first := range []string{r.Profile, r.EventsProfile} {
			for next := first; next != ""; next = c.Profiles[next].Fallback {
				if r.Sandbox == "claude" && c.Profiles[next].Agent != "claude" {
					return fieldError(path, prefix+".sandbox", "claude sandbox requires claude in the entire fallback chain")
				}
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
			return coreadapter.Profile{Name: next, Backend: p.Agent, Model: p.Model, Effort: p.Effort, Timeout: timeout, MaxTurns: p.MaxTurns, CostLimitUSD: c.Budget.SessionLimitUSD()}, c.RoleExecution(role), err
		}
	}
	return coreadapter.Profile{}, coreadapter.ExecutionSettings{}, fmt.Errorf("profile %q is not in role %q's fallback chain", profile, role)
}

const maxSocketPath = 103

func validateSocket(path string) error {
	if len(path) > maxSocketPath {
		return fmt.Errorf("socket path exceeds the %d-byte Unix socket limit; choose a shorter root/path", maxSocketPath)
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

// The values of a project's delivery setting, which says how a clean
// workstream leaves the project.
const (
	// DeliveryPullRequest delivers a workstream as a pull request.
	DeliveryPullRequest = "pull-request"
	// DeliveryJev opts the project in to landing a workstream directly on its
	// base branch.
	DeliveryJev = "jev"
)

// Lands reports whether the project has opted in to landing workstreams on
// its base branch rather than delivering only pull requests.
func (p Project) Lands() bool { return p.Delivery == DeliveryJev }

// PushRepository names the repository receiving feature branches.
func (p Project) PushRepository() string {
	if p.Fork != "" {
		return p.Fork
	}
	return p.Upstream
}
