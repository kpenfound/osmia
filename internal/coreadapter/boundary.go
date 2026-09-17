package coreadapter

import (
	"context"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/core/agent"
)

// Engine starts sessions inside the boundary their grants describe.
// *agent.Runner implements it: Verify returns the turn core would run for a
// request, and Run verifies the same request again before starting anything.
type Engine interface {
	Verify(agent.Request) (*agent.Turn, error)
	Run(context.Context, agent.Request) (*agent.Result, error)
}

var _ Engine = (*agent.Runner)(nil)

// CoreExecutor binds one turn to a service-selected isolation and runs it
// through core with grants built from that isolation alone: the view as the
// only writable or readable mount besides the session directory, the service
// environment as the complete allowlist, the scoped MCP servers as the only
// tools, and no VCS. Only container sessions are accepted, because a host
// session reads the whole filesystem.
type CoreExecutor struct {
	Required Isolation
	Runner   Engine
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
	if settings.Mode != agent.SandboxContainer {
		return unsupported("isolation mode", "requires a container; a host session reads the whole filesystem")
	}
	if settings.Image == "" {
		return unsupported("container", "image is required")
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
		case "LANG", "LC_ALL", "TZ", "OSMIA_MCP_TOKEN":
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

func (e CoreExecutor) Run(ctx context.Context, req agent.Request, settings ExecutionSettings) (*agent.Result, error) {
	if err := e.Check(ctx, e.Required, settings); err != nil {
		return nil, err
	}
	iso := e.Required
	if req.SessionDir == "" {
		return nil, unsupported("execution request", "session directory is required")
	}
	if err := os.MkdirAll(req.SessionDir, 0o700); err != nil {
		return nil, err
	}
	grants, err := coreGrants(iso, req.SessionDir, slices.Collect(maps.Keys(req.Profile.MCP)))
	if err != nil {
		return nil, err
	}
	// Core refuses whatever the grants do not cover. These are the request
	// fields grants do not describe; they also protect callers that invoke
	// Run without the normal turn translator.
	if req.Workspace == nil || req.Workspace.Directory() != iso.Workspace.Directory || req.Workspace.VCS() != nil ||
		len(req.VCSEnv) != 0 || len(req.VCSContainerEnv) != 0 || len(req.Profile.Skills) != 0 || req.Profile.ContainerUseEnvironment != "" ||
		len(req.Profile.SandboxDomains) != 0 || req.Profile.Sandbox != settings.Mode || req.Profile.SandboxImage != settings.Image ||
		!maps.Equal(req.Env, iso.Environment) || !slices.Equal(req.Profile.AllowedTools, AllowedTools(slices.Collect(maps.Keys(req.Profile.MCP)), iso.Capabilities.Tools)) ||
		(req.Grants != nil && !reflect.DeepEqual(*req.Grants, grants)) {
		return nil, unsupported("execution request", "request widens the service boundary")
	}
	for _, endpoint := range req.Profile.MCP {
		parsed, parseErr := url.Parse(endpoint.URL)
		if parseErr != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || endpoint.Type != "http" || endpoint.Command != "" || len(endpoint.Args) != 0 || len(endpoint.Env) != 0 || len(endpoint.EnvVars) != 0 || len(endpoint.Headers) != 0 || endpoint.BearerTokenEnv != "OSMIA_MCP_TOKEN" || req.Env["OSMIA_MCP_TOKEN"] == "" {
			return nil, unsupported("MCP", "only service-authenticated HTTP endpoints are permitted")
		}
	}
	req.Grants = &grants
	turn, err := e.Runner.Verify(req)
	if err != nil {
		return nil, &UnsupportedError{"grants", err.Error()}
	}
	if err = verifiedTurn(turn, iso, grants); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return e.Runner.Run(ctx, req)
}

// coreGrants are the complete capabilities of a turn in iso: the view with its
// access, the session directory read-only, the service environment and one
// MCP server grant per scoped endpoint, without built-in tools or VCS.
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
	for _, server := range slices.Sorted(slices.Values(servers)) {
		grants.Tools = append(grants.Tools, "mcp__"+server)
	}
	return grants, nil
}

// verifiedTurn refuses a turn whose effective boundary differs from the one
// the grants describe: VCS or a built-in tool granted, a VCS executable left
// on PATH, a variable beyond the service environment and the container's
// HOME, or a bind outside the granted mounts.
func verifiedTurn(turn *agent.Turn, iso Isolation, grants agent.Grants) error {
	if turn == nil {
		return unsupported("grant verification", "engine verified no turn")
	}
	if turn.VCS || turn.Tools == nil || len(turn.Tools) != 0 || len(turn.WriteDirs) != 0 {
		return unsupported("grant verification", "turn grants VCS or additional tools")
	}
	for _, name := range agent.VCSExecutables {
		if !slices.Contains(turn.DeniedExecutables, name) {
			return unsupported("grant verification", "VCS executable "+name+" is not denied")
		}
	}
	env := map[string]string{}
	for _, kv := range turn.Env {
		key, value, _ := strings.Cut(kv, "=")
		env[key] = value
	}
	delete(env, "HOME")
	if !maps.Equal(env, iso.Environment) {
		return unsupported("grant verification", "turn environment differs from the service environment")
	}
	aliases := map[string]agent.Mount{}
	for _, m := range grants.Mounts {
		real, err := filepath.EvalSymlinks(m.Path)
		if err != nil {
			return err
		}
		aliases[m.Path] = agent.Mount{Path: real, Access: m.Access}
		aliases[real] = agent.Mount{Path: real, Access: m.Access}
	}
	view := false
	for _, bind := range turn.Binds {
		granted, ok := aliases[bind.Destination]
		if !ok || granted.Path != bind.Source || granted.Access != bind.Access {
			return unsupported("grant verification", "container bind "+bind.Destination+" is not granted")
		}
		view = view || bind.Destination == iso.Workspace.Directory
	}
	if !view {
		return unsupported("grant verification", "container does not bind the view")
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
