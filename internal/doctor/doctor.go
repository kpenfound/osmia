// Package doctor checks what the service needs before it starts: the root,
// the configuration, the tools turns and workspaces run, each project's clone
// and remotes, GitHub access and the state the service opens on startup.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/engine"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// Status is the outcome of one check.
type Status string

const (
	Pass Status = "pass"
	// Warn is something that will probably bite but does not stop the service.
	Warn Status = "warn"
	// Fail is something that stops the service from starting or its work
	// from running.
	Fail Status = "fail"
)

// The groups checks are reported under, in report order.
const (
	GroupRoot      = "root"
	GroupConfig    = "config"
	GroupToolchain = "toolchain"
	GroupProjects  = "projects"
	GroupGitHub    = "github"
	GroupState     = "state"
)

// Check is one finding. Remediation says what fixes a warning or failure.
type Check struct {
	Group       string `json:"group"`
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

// Options are what doctor checks and the boundaries it reaches the host
// through. Every nil boundary uses the host's.
type Options struct {
	Root config.Root
	// Home resolves ~ in configured paths.
	Home string
	// LookPath finds an executable.
	LookPath func(string) (string, error)
	// Command runs an executable doctor found and returns its standard
	// output.
	Command func(ctx context.Context, path string, args ...string) (string, error)
	// EngineCommand runs engine probes with an explicit environment. When nil,
	// a supplied Command also handles these probes; otherwise the host is used.
	EngineCommand engine.Command
	// CheckJJ checks the jj on PATH.
	CheckJJ func(context.Context) (string, error)
	// TotalMemory returns the host's total memory in bytes, which the
	// sandbox memory estimate is compared with; nil reads the host's.
	TotalMemory func() (uint64, error)
	// Token is the GitHub token the service reads from GITHUB_TOKEN.
	Token string
	// Getenv reads the environment the service would start with, such as
	// the Jev API key; nil reads the host's.
	Getenv func(string) string
	// GitHubAPI is the GitHub REST API base URL; empty is
	// https://api.github.com.
	GitHubAPI string
	HTTP      *http.Client
}

const commandTimeout = 30 * time.Second

const lockFile = ".service.lock"

// Run runs every check and returns them in group order. Checks that need
// something an earlier check found missing are left out. While no service
// owns the root, doctor holds its ownership lock and opens each project's
// trace and the runtime state the way serve does on startup; while one does,
// those checks are left to the running service.
func Run(ctx context.Context, o Options) []Check {
	if o.EngineCommand == nil && o.Command != nil {
		o.EngineCommand = func(ctx context.Context, path string, _ []string, args ...string) (string, error) {
			return o.Command(ctx, path, args...)
		}
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Command == nil {
		o.Command = command
	}
	if o.CheckJJ == nil {
		o.CheckJJ = func(ctx context.Context) (string, error) { return workspace.CheckJJ(ctx, "") }
	}
	if o.TotalMemory == nil {
		o.TotalMemory = hostMemory
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.GitHubAPI == "" {
		o.GitHubAPI = "https://api.github.com"
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: commandTimeout}
	}
	r := &run{Options: o}
	lock := r.root()
	if lock != nil {
		defer unlock(lock)
	}
	cfg, projects := r.config()
	r.toolchain(ctx, cfg)
	if cfg != nil {
		r.engine(ctx, cfg)
	}
	r.sandboxMemory(cfg)
	r.socket(cfg)
	r.web(cfg)
	r.jev(cfg)
	for _, p := range projects {
		r.project(ctx, p)
	}
	r.github(ctx, cfg, projects)
	if lock != nil && cfg != nil {
		r.state(cfg, projects)
	}
	slices.SortStableFunc(r.checks, func(a, b Check) int { return groupOrder(a.Group) - groupOrder(b.Group) })
	return r.checks
}

// Failed reports whether any check failed.
func Failed(checks []Check) bool {
	return slices.ContainsFunc(checks, func(c Check) bool { return c.Status == Fail })
}

func groupOrder(g string) int {
	return slices.Index([]string{GroupRoot, GroupConfig, GroupToolchain, GroupProjects, GroupGitHub, GroupState}, g)
}

type run struct {
	Options
	checks    []Check
	running   bool
	rootReady bool
}

func (r *run) add(group, name string, status Status, detail, remediation string) {
	if status == Pass {
		remediation = ""
	}
	r.checks = append(r.checks, Check{Group: group, Name: name, Status: status, Detail: detail, Remediation: remediation})
}

func (r *run) root() *os.File {
	dir := r.Root.String()
	info, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		r.add(GroupRoot, "root directory", Pass, dir+" does not exist yet; serve creates it", "")
		return nil
	case err != nil:
		r.add(GroupRoot, "root directory", Fail, dir+": "+reason(err), "make the root accessible to this user, or choose another with --root")
		return nil
	case !info.IsDir():
		r.add(GroupRoot, "root directory", Fail, dir+" is not a directory", "choose a directory with --root")
		return nil
	}
	if err := syscall.Access(dir, 0x2|0x1); err != nil {
		r.add(GroupRoot, "root directory", Fail, dir+" is not writable", "make the root writable by this user, or choose another with --root")
		return nil
	}
	r.rootReady = true
	r.add(GroupRoot, "root directory", Pass, dir, "")
	r.serviceLog()
	path := filepath.Join(dir, lockFile)
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		r.add(GroupRoot, "ownership lock", Fail, path+": "+reason(err), "remove "+path+" if it is not a regular file; the service recreates it")
		return nil
	}
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		lock.Close()
		r.add(GroupRoot, "ownership lock", Fail, path+" is not a regular file", "remove "+path+"; the service recreates it")
		return nil
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		r.running = true
		r.add(GroupRoot, "ownership lock", Warn, "a service owns this root, so serve would refuse to start another; osmia status reports its state", "stop it with osmia stop before starting the service again")
		return nil
	}
	r.add(GroupRoot, "ownership lock", Pass, "no service owns this root", "")
	return lock
}

func unlock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

func (r *run) serviceLog() {
	path := filepath.Join(r.Root.String(), "service.log")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		r.add(GroupRoot, "service log", Warn, path+" is not a private regular file, so serve --detach refuses to start", "chmod 600 "+path+", or remove it if it is not a regular file")
		return
	}
	r.add(GroupRoot, "service log", Pass, path, "")
}

func (r *run) config() (*config.Config, []config.Project) {
	if !r.rootReady {
		return nil, nil
	}
	path, _ := r.Root.Config()
	cfg, err := config.LoadTopLevel(r.Root.Options(r.Home))
	if err != nil {
		remedy := "fix the field named and run osmia doctor again; see the configuration reference"
		if errors.Is(err, os.ErrNotExist) {
			remedy = "create " + path + " with version = 1 and at least one profile; see the configuration reference"
		}
		r.add(GroupConfig, "config.toml", Fail, configReason(err), remedy)
		return nil, nil
	}
	r.add(GroupConfig, "config.toml", Pass, fmt.Sprintf("%s (%d profiles)", path, len(cfg.Profiles)), "")
	for _, w := range cfg.Warnings {
		r.add(GroupConfig, "config.toml "+w.Field, Warn, w.Field+" is deprecated; use "+w.Replacement, "rename "+w.Field+" to "+w.Replacement+" in "+path+"; both forms in one file are refused")
	}
	return cfg, r.projects(cfg)
}

func (r *run) projects(cfg *config.Config) []config.Project {
	store := projectstore.Open(cfg.Root)
	ids, err := store.Projects()
	if err != nil {
		r.add(GroupConfig, "project store", Fail, reason(err), "fix or move aside "+filepath.Join(cfg.Root.String(), "project-store")+"; it holds the project registry and every project's settings")
		return nil
	}
	var projects []config.Project
	for _, id := range ids {
		file, _, err := store.Load(cfg, r.Home, id)
		if err != nil {
			r.add(GroupConfig, "project "+string(id), Fail, storedReason(err), "correct the stored setting named with osmia project settings "+string(id)+" set NAME=VALUE while the service is stopped")
			continue
		}
		projects = append(projects, r.projectLoaded(file.Project))
	}
	return projects
}

func (r *run) projectLoaded(p config.Project) config.Project {
	r.add(GroupConfig, "project "+string(p.ID), Pass, fmt.Sprintf("%s (%s)", p.Name, p.Upstream), "")
	return p
}

func storedReason(err error) string {
	var field *config.FieldError
	if !errors.As(err, &field) {
		return "the stored settings cannot be loaded"
	}
	if field.Field == "" {
		return field.Reason
	}
	return field.Field + ": " + field.Reason
}

func configReason(err error) string {
	var field *config.FieldError
	if errors.As(err, &field) {
		return field.Error()
	}
	return "cannot be loaded"
}

func (r *run) toolchain(ctx context.Context, cfg *config.Config) {
	r.tool(ctx, "git", Fail, "install git; every workspace, landing and delivery runs it", "--version")
	r.jj(ctx, cfg)
	if cfg == nil {
		return
	}
	var agents []string
	sandboxes := map[string]bool{}
	for _, role := range cfg.Roles {
		sandboxes[role.Sandbox] = true
		for next := role.Profile; next != ""; next = cfg.Profiles[next].Fallback {
			if a := cfg.Profiles[next].Agent; !slices.Contains(agents, a) {
				agents = append(agents, a)
			}
		}
	}
	slices.Sort(agents)
	for _, a := range agents {
		r.agentTool(ctx, a, "install "+a+" or change the profiles that name agent = \""+a+"\"; turns on those profiles fail")
	}
	if sandboxes["container"] {
		r.containerTool(ctx, "install and start "+agent.ContainerEngine+"; roles with sandbox = \"container\" run their turns in it")
	}
	if sandboxes["sbx"] {
		r.sbxTool(ctx, "install Docker Sandboxes; roles with sandbox = \"sbx\" run their turns in it")
	}
	r.daggerTool(ctx, Warn, "install dagger; the service runs each candidate's checks with dagger check before review")
}

func (r *run) agentTool(ctx context.Context, name, remedy string) {
	r.tool(ctx, name, Fail, remedy, "--version")
}

func (r *run) containerTool(ctx context.Context, remedy string) {
	r.tool(ctx, agent.ContainerEngine, Fail, remedy, "version", "--format", "{{.Server.Version}}")
}

func (r *run) sbxTool(ctx context.Context, remedy string) {
	r.tool(ctx, agent.SandboxCLI, Fail, remedy, "version")
}

func (r *run) daggerTool(ctx context.Context, status Status, remedy string) {
	r.tool(ctx, "dagger", status, remedy, "version")
}

func (r *run) tool(ctx context.Context, name string, status Status, remedy string, args ...string) {
	path, err := r.LookPath(name)
	if err != nil {
		r.add(GroupToolchain, name, status, name+" is not on PATH", remedy)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	out, err := r.Command(ctx, path, args...)
	if err != nil {
		r.add(GroupToolchain, name, status, fmt.Sprintf("%s %s: %s", path, strings.Join(args, " "), reason(err)), remedy)
		return
	}
	r.add(GroupToolchain, name, Pass, fmt.Sprintf("%s (%s)", path, firstLine(out)), "")
}

func (r *run) jj(ctx context.Context, cfg *config.Config) {
	setting := config.WorkspacesAuto
	if cfg != nil {
		setting = cfg.Workspaces
	}
	if setting == config.WorkspacesGit {
		r.add(GroupToolchain, "jj", Pass, "not used: workspaces = \"git\"", "")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	version, err := r.CheckJJ(ctx)
	if err == nil {
		r.add(GroupToolchain, "jj", Pass, "jj "+version, "")
		return
	}
	remedy := fmt.Sprintf("install jj %s or later", workspace.MinimumJJ)
	if setting == config.WorkspacesJujutsu {
		r.add(GroupToolchain, "jj", Fail, reason(err), remedy+", or set workspaces = \"git\"")
		return
	}
	r.add(GroupToolchain, "jj", Warn, reason(err)+"; new workstreams use Git worktrees", remedy+" for Jujutsu workspaces, or set workspaces = \"git\"")
}

func (r *run) socket(cfg *config.Config) {
	if cfg == nil {
		return
	}
	path := cfg.Listen.Socket
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		r.add(GroupRoot, "socket", Pass, path+" is free", "")
		return
	}
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	switch {
	case err == nil:
		conn.Close()
		if !r.running {
			r.add(GroupRoot, "socket", Fail, "a process that does not own the root listens on "+path, "stop the process listening on "+path)
			return
		}
		r.add(GroupRoot, "socket", Pass, "the running service listens on "+path, "")
	case errors.Is(err, syscall.ECONNREFUSED):
		r.add(GroupRoot, "socket", Pass, path+" is stale; serve removes it", "")
	default:
		r.add(GroupRoot, "socket", Fail, "cannot tell whether "+path+" is stale: "+reason(err), "make the socket accessible to this user, or remove it once no process uses it")
	}
}

func (r *run) web(cfg *config.Config) {
	if cfg == nil || cfg.Listen.Web == "" || r.running {
		return
	}
	l, err := net.Listen("tcp", cfg.Listen.Web)
	if err != nil {
		r.add(GroupConfig, "listen.web", Fail, "cannot bind "+cfg.Listen.Web+": "+reason(err), "stop the process using "+cfg.Listen.Web+" or change listen.web")
		return
	}
	l.Close()
	r.add(GroupConfig, "listen.web", Pass, cfg.Listen.Web+" can be bound", "")
}

func (r *run) jev(cfg *config.Config) {
	if cfg == nil {
		return
	}
	name := cfg.Jev.APIKeyEnv
	if r.Getenv(name) == "" {
		r.add(GroupConfig, "jev", Fail, "Jev is unconfigured: "+name+" is not set, so every judgment falls back and every check runs", "export "+name+" before osmia serve")
		return
	}
	r.add(GroupConfig, "jev", Pass, "Jev is configured with "+name+" set for "+cfg.Jev.Model, "")
}

func (r *run) project(ctx context.Context, p config.Project) {
	name := p.Name
	info, err := os.Stat(p.Clone)
	if err != nil || !info.IsDir() {
		r.add(GroupProjects, name+" clone", Fail, p.Clone+" is not a directory", "clone "+p.Upstream+" to "+p.Clone+", or point the project at its clone with osmia project settings "+string(p.ID)+" set clone=PATH while the service is stopped")
		return
	}
	short, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	git := exec.CommandContext(short, "git", "-C", p.Clone, "rev-parse", "--is-inside-work-tree")
	git.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	if out, err := git.Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		r.add(GroupProjects, name+" clone", Fail, p.Clone+" is not a Git work tree", "clone "+p.Upstream+" to "+p.Clone+", or point the project at its clone with osmia project settings "+string(p.ID)+" set clone=PATH while the service is stopped")
		return
	}
	r.add(GroupProjects, name+" clone", Pass, p.Clone, "")
	g := &workspace.Git{Clone: p.Clone}
	r.remote(ctx, g, p.ID, name+" upstream", p.Upstream, p.BaseBranch, p.Clone)
	if push := p.PushRepository(); push != p.Upstream {
		r.remote(ctx, g, p.ID, name+" fork", push, "", p.Clone)
	}
}

func (r *run) remote(ctx context.Context, g *workspace.Git, id config.ProjectID, check, repository, branch, clone string) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	remote, err := g.Remote(ctx, repository)
	if err != nil {
		r.add(GroupProjects, check, Fail, "the clone has no remote whose URL names "+repository, "git -C "+clone+" remote add <name> git@github.com:"+repository+".git")
		return
	}
	probe := branch
	if probe == "" {
		probe = "HEAD"
	}
	_, exists, err := g.RemoteBranch(ctx, remote, probe)
	if err != nil {
		r.add(GroupProjects, check, Fail, fmt.Sprintf("remote %q does not answer: %s", remote, firstLine(err.Error())), "check the remote URL and your Git credentials: git -C "+clone+" ls-remote "+remote)
		return
	}
	if branch != "" && !exists {
		r.add(GroupProjects, check, Fail, fmt.Sprintf("remote %q has no branch %s", remote, branch), "set base_branch to a branch of "+repository+" with osmia project settings "+string(id)+" set base_branch=BRANCH while the service is stopped")
		return
	}
	detail := fmt.Sprintf("remote %q answers", remote)
	if branch != "" {
		detail = fmt.Sprintf("remote %q answers, branch %s exists", remote, branch)
	}
	r.add(GroupProjects, check, Pass, detail, "")
}

func (r *run) github(ctx context.Context, cfg *config.Config, projects []config.Project) {
	creation := r.creationConfigured(cfg)
	if r.Token == "" {
		r.add(GroupGitHub, "GITHUB_TOKEN", Warn, "GITHUB_TOKEN is not set", "export GITHUB_TOKEN before osmia serve; hand-ins from issue URLs and pull requests need it")
		if creation {
			r.add(GroupGitHub, "creation account", Warn, "cannot confirm that the token's account is projects.repo_owner: GITHUB_TOKEN is not set", "export GITHUB_TOKEN before osmia serve; project creation needs it")
			r.add(GroupGitHub, "creation permission", Warn, "cannot confirm that the token can create repositories: GITHUB_TOKEN is not set", "export GITHUB_TOKEN before osmia serve; project creation needs it")
		}
		return
	}
	var user struct {
		Login string `json:"login"`
	}
	header, err := r.getHeader(ctx, "/user", &user)
	if err != nil {
		r.add(GroupGitHub, "GITHUB_TOKEN", Fail, "GitHub refuses the token: "+reason(err), "replace GITHUB_TOKEN with a valid token")
		if creation {
			r.add(GroupGitHub, "creation account", Warn, "cannot confirm that the token's account is projects.repo_owner: GitHub refuses the token", "replace GITHUB_TOKEN with a valid token")
			r.add(GroupGitHub, "creation permission", Warn, "cannot confirm that the token can create repositories: GitHub refuses the token", "replace GITHUB_TOKEN with a valid token")
		}
		return
	}
	r.add(GroupGitHub, "GITHUB_TOKEN", Pass, "authenticates as "+user.Login, "")
	if creation {
		r.creationToken(cfg.ProjectCreation.RepoOwner, user.Login, header)
	}
	for _, p := range projects {
		repositories := []string{p.Upstream}
		if push := p.PushRepository(); push != p.Upstream {
			repositories = append(repositories, push)
		}
		for _, repo := range repositories {
			var out struct {
				FullName string `json:"full_name"`
			}
			owner, name, _ := strings.Cut(repo, "/")
			if err := r.get(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), &out); err != nil {
				r.add(GroupGitHub, p.Name+" "+repo, Fail, "cannot read "+repo+": "+reason(err), "grant GITHUB_TOKEN read access to "+repo)
				continue
			}
			r.add(GroupGitHub, p.Name+" "+repo, Pass, "readable by "+user.Login, "")
		}
	}
}

func (r *run) creationConfigured(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if missing := cfg.ProjectCreation.MissingSetting(); missing != "" {
		r.add(GroupGitHub, "project creation", Pass, "not configured: "+missing+" is not set, so new projects cannot be created", "")
		return false
	}
	r.add(GroupGitHub, "project creation", Pass, "configured: new repositories are created under "+cfg.ProjectCreation.RepoOwner+" and cloned into "+cfg.ProjectCreation.CloneDir, "")
	return true
}

func (r *run) creationToken(repoOwner, login string, header http.Header) {
	if strings.EqualFold(login, repoOwner) {
		r.add(GroupGitHub, "creation account", Pass, "the token's account is projects.repo_owner ("+login+")", "")
	} else {
		r.add(GroupGitHub, "creation account", Warn, "the token authenticates as "+login+" but projects.repo_owner is "+repoOwner+"; repositories are always created under the token's own account", "set projects.repo_owner to "+login+", or use a token of "+repoOwner)
	}
	scopes := header.Get("X-OAuth-Scopes")
	if scopes == "" {
		r.add(GroupGitHub, "creation permission", Warn, "cannot confirm that the token can create repositories: GitHub reported no scopes, as for a fine-grained token", "a fine-grained token needs Administration (or repository creation) write plus Pull requests and Metadata, with access to all repositories; a classic token needs the repo scope")
		return
	}
	if slices.ContainsFunc(strings.Split(scopes, ","), func(scope string) bool { return strings.TrimSpace(scope) == "repo" }) {
		r.add(GroupGitHub, "creation permission", Pass, "the token has the repo scope", "")
		return
	}
	r.add(GroupGitHub, "creation permission", Warn, "the token's scopes ("+firstLine(scopes)+") do not include repo, so it cannot create repositories", "create a classic token with the repo scope, or a fine-grained token with Administration write, Pull requests and Metadata for all repositories")
}

func (r *run) get(ctx context.Context, path string, out any) error {
	_, err := r.getHeader(ctx, path, out)
	return err
}

func (r *run) getHeader(ctx context.Context, path string, out any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(r.GitHubAPI, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Header, json.NewDecoder(resp.Body).Decode(out)
}

func (r *run) state(cfg *config.Config, projects []config.Project) {
	workstreams := map[config.ProjectID][]config.WorkstreamID{}
	for _, p := range projects {
		check := p.Name + " trace"
		directory, err := cfg.Root.ProjectTrace(p.ID)
		if err != nil {
			r.add(GroupState, check, Fail, reason(err), "")
			continue
		}
		if exists, err := trace.HasTraceDatabase(directory); err != nil {
			r.add(GroupState, check, Fail, reason(err), "")
			continue
		} else if !exists {
			r.add(GroupState, check, Warn, "the project has no trace, so it runs idle", "add it again with osmia project add")
			continue
		}
		repository, err := trace.Open(cfg.Root, p)
		if err != nil {
			if repository != nil {
				repository.Close()
			}
			if errors.Is(err, trace.ErrIntegrity) {
				r.add(GroupState, check, Fail, reason(err), "restore the damaged trace database from a backup; serve refuses to start while it does not open")
				continue
			}
			r.add(GroupState, check, Fail, directory+": "+reason(err), "repair or restore the trace at "+directory+"; serve refuses to start while it does not open")
			continue
		}
		streams, err := repository.Workstreams()
		repository.Close()
		if err != nil {
			r.add(GroupState, check, Fail, directory+": "+reason(err), "repair or restore the trace at "+directory)
			continue
		}
		workstreams[p.ID] = streams
		r.add(GroupState, check, Pass, fmt.Sprintf("%s (%d workstreams)", directory, len(streams)), "")
	}
	loaded := cfg.WithLoadedProjects(projects)
	path, _ := cfg.Root.Runtime()
	st, diagnostics, err := runtime.Open(runtime.Inputs{Config: loaded, Workstreams: workstreams})
	if err != nil {
		r.add(GroupState, "runtime.json", Fail, reason(err), "fix or move aside "+path+"; it holds pauses, priorities and profile overrides")
		return
	}
	st.Close()
	if len(diagnostics) > 0 {
		r.add(GroupState, "runtime.json", Warn, fmt.Sprintf("%d stale runtime controls", len(diagnostics)), "start the service and review them with osmia config")
		return
	}
	r.add(GroupState, "runtime.json", Pass, path, "")
}

func command(ctx context.Context, path string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return "", fmt.Errorf("%w: %s", err, firstLine(string(exit.Stderr)))
	}
	return string(out), err
}

func reason(err error) string { return firstLine(err.Error()) }

func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 120 {
				line = line[:117] + "..."
			}
			return line
		}
	}
	return ""
}
