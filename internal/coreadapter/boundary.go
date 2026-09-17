package coreadapter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/vcs"
)

// Engine hands out the core enforcer that holds turns of one set of execution
// settings to their grants.
type Engine interface {
	Enforcer(ExecutionSettings) (agent.Enforcer, error)
}

// CoreEngine is the production Engine: every enforcer runs its turns with
// Runner.
type CoreEngine struct{ Runner agent.Runner }

var _ Engine = CoreEngine{}

func (e CoreEngine) Enforcer(settings ExecutionSettings) (agent.Enforcer, error) {
	return NewEnforcer(e.Runner, settings)
}

// NewEnforcer builds the enforcer of a role's resolved execution settings:
// agent.NewHostNone for "none", agent.NewHostClaude for "claude" and
// agent.NewContainer with the settings' image for "container". A host mode
// with an image, a container without one, or any other mode is refused with
// an UnsupportedError. Whether the platform can enforce the mode is decided by
// the enforcer's Prepare.
func NewEnforcer(r agent.Runner, settings ExecutionSettings) (agent.Enforcer, error) {
	if err := checkMode(settings); err != nil {
		return nil, err
	}
	switch settings.Mode {
	case agent.SandboxClaude:
		return agent.NewHostClaude(r), nil
	case agent.SandboxContainer:
		return agent.NewContainer(r, settings.Image), nil
	}
	return agent.NewHostNone(r), nil
}

// checkMode refuses settings no enforcer is built for: a host mode with an
// image, a container without one, or a mode other than none, claude and
// container.
func checkMode(settings ExecutionSettings) error {
	switch settings.Mode {
	case agent.SandboxNone, agent.SandboxClaude:
		if settings.Image != "" {
			return unsupported("isolation mode", "a host session runs no image")
		}
		return nil
	case agent.SandboxContainer:
		if settings.Image == "" {
			return unsupported("container", "image is required")
		}
		return nil
	}
	return unsupported("isolation mode", fmt.Sprintf("%q is not none, claude or container", settings.Mode))
}

// CoreExecutor binds one turn to a service-selected isolation and runs it
// through a core enforcer with grants built from that isolation alone: the
// view as the only mount besides the session's own directory, the service
// environment as the complete allowlist, the scoped MCP servers as the only
// tools, and no VCS. Before the turn runs, the prepared session's policy must
// match those grants and leave every Pinned directory unwritable.
type CoreExecutor struct {
	Required Isolation
	Runner   Engine
	// Pinned are the directories of the revisions the view was made from.
	Pinned []string
}

func (e CoreExecutor) Check(ctx context.Context, iso Isolation, settings ExecutionSettings) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !reflect.DeepEqual(iso, e.Required) {
		return unsupported("isolation", "turn differs from service grant")
	}
	if e.Runner == nil {
		return unsupported("execution engine", "no core runner supplied")
	}
	if err := checkMode(settings); err != nil {
		return err
	}
	if len(settings.Mounts) != 0 || len(settings.Domains) != 0 {
		return unsupported("execution overrides", "extra mounts and network domains are not granted")
	}
	if !iso.DenyVCS || !iso.DenyInheritedEnvironment || !iso.DenyDeliveryCredentials {
		return unsupported("isolation", "mandatory denials are missing")
	}
	if len(iso.Credentials) != 0 {
		return unsupported("credentials", "unresolved references are not permitted")
	}
	if iso.Workspace.Access != ReadOnly && iso.Workspace.Access != ReadWrite {
		return unsupported("workspace", "invalid access")
	}
	if iso.Workspace.Access == ReadOnly && (iso.Capabilities.WriteFiles || iso.Capabilities.Execute || iso.Capabilities.Network) {
		return unsupported("read-only role", "write, execute and fetch are forbidden")
	}
	if iso.Workspace.Access == ReadWrite && !iso.Capabilities.WriteFiles {
		return unsupported("workspace", "writable mount requires a file-write grant")
	}
	if err := PublicEnvironment(iso.Environment); err != nil {
		return err
	}
	return validateFileTree(iso.Workspace.Directory)
}

// PublicEnvironment admits only literal public locale settings and a service-
// generated scoped MCP token. It never reads or expands process environment.
func PublicEnvironment(env map[string]string) error {
	for key, value := range env {
		if strings.ContainsRune(value, 0) {
			return unsupported("environment", "NUL value")
		}
		switch key {
		case "LANG", "LC_ALL", "TZ", TokenEnvironment:
		default:
			return unsupported("environment", "variable is not on the service allowlist")
		}
	}
	return nil
}

func validateFileTree(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return unsupported("workspace", "requires a canonical absolute directory")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if resolved != dir {
		return unsupported("workspace", "symlinked workspace root")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return unsupported("workspace", "not a directory")
	}
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		switch strings.ToLower(entry.Name()) {
		case ".git", ".jj", ".hg", ".svn":
			return unsupported("workspace", "VCS metadata is not permitted")
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return unsupported("workspace", "symlinks and special files are not permitted")
		}
		return nil
	})
}

func (e CoreExecutor) Run(ctx context.Context, req agent.Request, settings ExecutionSettings) (result *agent.Result, err error) {
	if err := e.Check(ctx, e.Required, settings); err != nil {
		return nil, err
	}
	iso := e.Required
	if req.SessionDir == "" {
		return nil, unsupported("execution request", "session directory is required")
	}
	grants, err := coreGrants(iso, req.SessionDir, slices.Collect(maps.Keys(req.Profile.MCP)))
	if err != nil {
		return nil, err
	}
	// Core refuses whatever the grants do not cover. These are the request
	// fields grants do not describe; they also protect callers that invoke
	// Run without the normal turn translator.
	if req.Workspace == nil || req.Workspace.Directory() != iso.Workspace.Directory || req.Workspace.VCS() != nil ||
		len(req.VCSEnv) != 0 || len(req.VCSContainerEnv) != 0 || len(req.ContainerEnv) != 0 || len(req.Profile.Skills) != 0 || req.Profile.ContainerUseEnvironment != "" ||
		len(req.Profile.SandboxDomains) != 0 || req.Profile.Sandbox != settings.Mode || req.Profile.SandboxImage != settings.Image ||
		!maps.Equal(req.Env, iso.Environment) || !slices.Equal(req.Profile.AllowedTools, AllowedTools(slices.Collect(maps.Keys(req.Profile.MCP)), iso.Capabilities.Tools)) ||
		(req.Grants != nil && !reflect.DeepEqual(*req.Grants, grants)) {
		return nil, unsupported("execution request", "request widens the service boundary")
	}
	for _, endpoint := range req.Profile.MCP {
		parsed, parseErr := url.Parse(endpoint.URL)
		if parseErr != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || endpoint.Type != "http" || endpoint.Command != "" || len(endpoint.Args) != 0 || len(endpoint.Env) != 0 || len(endpoint.EnvVars) != 0 || len(endpoint.Headers) != 0 || endpoint.BearerTokenEnv != TokenEnvironment || req.Env[TokenEnvironment] == "" {
			return nil, unsupported("MCP", "only service-authenticated HTTP endpoints are permitted")
		}
	}
	if err = os.MkdirAll(req.SessionDir, 0o700); err != nil {
		return nil, err
	}
	if iso.Workspace.Access == ReadOnly {
		// Core runs a session in a writable directory; a read-only view is
		// mounted beside an empty scratch directory the session starts in.
		scratch := grants.Mounts[len(grants.Mounts)-1].Path
		if err = os.Mkdir(scratch, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		req.Workspace = vcs.Directory(scratch)
	}
	req.Grants = &grants
	enforcer, err := e.Runner.Enforcer(settings)
	if err != nil {
		return nil, refusal(err)
	}
	session, err := enforcer.Prepare(ctx, grants)
	if err != nil {
		return nil, refusal(err)
	}
	defer func() { err = errors.Join(err, session.Release(context.WithoutCancel(ctx))) }()
	if err = checkPolicy(session.Policy(), iso, settings, grants, e.Pinned); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	result, err = session.Run(ctx, req)
	return result, refusal(err)
}

// refusal wraps core's refusals of a turn in ErrUnsupported, keeping core's
// reason. A turn whose session policy changed since Prepare is one of them.
func refusal(err error) error {
	for _, refused := range []error{agent.ErrUnsupported, agent.ErrNotGranted, agent.ErrNoGrants, agent.ErrPolicyChanged} {
		if errors.Is(err, refused) {
			return fmt.Errorf("%w: %w", ErrUnsupported, err)
		}
	}
	return err
}

// scratchDirectory is the session subdirectory a read-only turn starts in.
const scratchDirectory = "work"

// coreGrants are the complete capabilities of a turn in iso: the view with its
// access, the session directory read-only, a writable scratch directory inside
// it when the view is read-only, the service environment and one MCP server
// grant per scoped endpoint, without built-in tools or VCS.
func coreGrants(iso Isolation, sessionDir string, servers []string) (agent.Grants, error) {
	access := agent.ReadOnly
	if iso.Workspace.Access == ReadWrite {
		access = agent.ReadWrite
	}
	session, err := filepath.Abs(sessionDir)
	if err != nil {
		return agent.Grants{}, err
	}
	grants := agent.Grants{
		Env:    slices.Sorted(maps.Keys(iso.Environment)),
		Tools:  []string{},
		Mounts: []agent.Mount{{Path: iso.Workspace.Directory, Access: access}, {Path: session, Access: agent.ReadOnly}},
	}
	if access == agent.ReadOnly {
		grants.Mounts = append(grants.Mounts, agent.Mount{Path: filepath.Join(session, scratchDirectory), Access: agent.ReadWrite})
	}
	for _, server := range slices.Sorted(slices.Values(servers)) {
		grants.Tools = append(grants.Tools, "mcp__"+server)
	}
	return grants, nil
}

// checkPolicy refuses a session whose policy is not the one the grants
// describe: another sandbox or image, a view that is not readable or whose
// write access differs from the isolation's, a writable pinned directory, VCS
// granted or a VCS executable left undenied, a built-in tool, or MCP servers
// other than the granted ones.
func checkPolicy(p agent.Policy, iso Isolation, settings ExecutionSettings, grants agent.Grants, pinned []string) error {
	if p.Sandbox != settings.Mode || p.Image != settings.Image {
		return unsupported("session policy", fmt.Sprintf("sandbox %q with image %q differs from the role's", p.Sandbox, p.Image))
	}
	view := iso.Workspace.Directory
	if !p.Reads(view) || p.Writes(view) != (iso.Workspace.Access == ReadWrite) {
		return unsupported("session policy", "view access differs from the grant")
	}
	for _, dir := range pinned {
		if p.Writes(dir) {
			return unsupported("session policy", "pinned revision "+dir+" is writable")
		}
	}
	if p.VCS {
		return unsupported("session policy", "VCS is granted")
	}
	for _, name := range agent.VCSExecutables {
		if !slices.Contains(p.DeniedExecutables, name) {
			return unsupported("session policy", "VCS executable "+name+" is not denied")
		}
	}
	if p.Tools == nil || len(p.Tools) != 0 {
		return unsupported("session policy", "built-in tools are granted")
	}
	var servers []string
	for _, tool := range grants.Tools {
		servers = append(servers, strings.TrimPrefix(tool, "mcp__"))
	}
	if !slices.Equal(p.MCPServers, servers) {
		return unsupported("session policy", "MCP servers differ from the granted ones")
	}
	return nil
}

// CheckResume asks the execution engine whether the saved session is available
// and compatible. It reads no transcript and launches nothing. An engine without
// this capability selects owned-log replay.
func (e CoreExecutor) CheckResume(ctx context.Context, previous, next Profile, session BackendSession) error {
	checker, ok := e.Runner.(ResumeChecker)
	if !ok {
		return unsupported("resume", "execution engine cannot verify saved session compatibility and availability")
	}
	return checker.CheckResume(ctx, previous, next, session)
}
