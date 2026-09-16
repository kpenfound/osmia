package coreadapter

import (
	"context"
	"errors"
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

// Mount is the complete host exposure for a session. Backend images and runtime
// support must be provided by the engine without additional host mounts.
type Mount struct {
	Source, Target string
	ReadOnly       bool
}

// BoundaryPolicy describes restrictions the engine must establish before launch.
// Network is model-tool network access; provider and scoped MCP transport remain
// engine-owned and must not expose a general fetch or proxy capability.
type BoundaryPolicy struct {
	Isolation                                       Isolation
	Mode, Image                                     string
	Mounts                                          []Mount
	NoVCS, NoHostEnvironment, NoDeliveryCredentials bool
	NoHostFiles, NoExtraTools, NoConfigDiscovery    bool
	NoPrivilegeEscalation                           bool
}

// IsolationEngine prepares a stopped execution boundary. It is trusted service
// code, not a model/tool callback. Inspect must report restrictions established
// by the OS/backend, not echo requested flags. Prepare owns partial-error cleanup.
// No production implementation is supplied by the pinned core runner.
type IsolationEngine interface {
	Prepare(context.Context, BoundaryPolicy) (IsolatedSession, error)
}
type IsolatedSession interface {
	Inspect(context.Context) (BoundaryPolicy, error)
	Run(context.Context, agent.Request) (*agent.Result, error)
	Release(context.Context) error
}

// BoundaryExecutor binds one turn to a service-selected policy. The engine must
// confine writes to the view, deny VCS executables (including absolute paths and
// shell indirection), and enforce tool permissions independent of model prompts.
// TODO: Remove this extra boundary adapter when busybees/core provides verified
// execution with complete environments, confined mounts and immutable tool grants.
type BoundaryExecutor struct {
	Required Isolation
	Engine   IsolationEngine
}

func (e BoundaryExecutor) Check(ctx context.Context, iso Isolation, settings ExecutionSettings) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !reflect.DeepEqual(iso, e.Required) {
		return unsupported("isolation", "turn differs from service grant")
	}
	if e.Engine == nil {
		return unsupported("isolation engine", "no enforcing host or container engine supplied")
	}
	if settings.Mode != "none" && settings.Mode != "container" {
		return unsupported("isolation mode", "requires host or container")
	}
	if settings.Mode == "container" && settings.Image == "" {
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

func (e BoundaryExecutor) Run(ctx context.Context, req agent.Request, settings ExecutionSettings) (result *agent.Result, err error) {
	if err = e.Check(ctx, e.Required, settings); err != nil {
		return nil, err
	}
	iso := e.Required
	// All ambient extension points in core's request are forbidden. This check
	// also protects callers that invoke Run without the normal turn translator.
	if req.Workspace == nil || req.Workspace.Directory() != iso.Workspace.Directory || req.Workspace.VCS() != nil || req.Profile.VCSAccess ||
		len(req.VCSEnv) != 0 || len(req.VCSContainerEnv) != 0 || len(req.ContainerEnv) != 0 || req.HostMCP != nil ||
		len(req.Profile.Env) != 0 || len(req.Profile.Skills) != 0 || req.Profile.Shell != "" || req.Profile.ContainerUseEnvironment != "" ||
		len(req.Profile.SandboxDomains) != 0 || req.Profile.Sandbox != settings.Mode || req.Profile.SandboxImage != settings.Image ||
		!maps.Equal(req.Env, iso.Environment) || !slices.Equal(req.Profile.AllowedTools, iso.Capabilities.Tools) {
		return nil, unsupported("execution request", "request widens the service boundary")
	}
	for _, endpoint := range req.Profile.MCP {
		parsed, parseErr := url.Parse(endpoint.URL)
		if parseErr != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || endpoint.Type != "http" || endpoint.Command != "" || len(endpoint.Args) != 0 || len(endpoint.Env) != 0 || len(endpoint.EnvVars) != 0 || len(endpoint.Headers) != 0 || endpoint.BearerTokenEnv != "OSMIA_MCP_TOKEN" || req.Env["OSMIA_MCP_TOKEN"] == "" {
			return nil, unsupported("MCP", "only service-authenticated HTTP endpoints are permitted")
		}
	}
	policy := BoundaryPolicy{Isolation: cloneIsolation(iso), Mode: settings.Mode, Image: settings.Image,
		Mounts: []Mount{{Source: iso.Workspace.Directory, Target: iso.Workspace.Directory, ReadOnly: iso.Workspace.Access == ReadOnly}},
		NoVCS:  true, NoHostEnvironment: true, NoDeliveryCredentials: true, NoHostFiles: true, NoExtraTools: true, NoConfigDiscovery: true, NoPrivilegeEscalation: true}
	session, err := e.Engine.Prepare(ctx, clonePolicy(policy))
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, unsupported("isolation engine", "no stopped session returned")
	}
	defer func() { err = errors.Join(err, session.Release(context.WithoutCancel(ctx))) }()
	actual, err := session.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(policy, actual) {
		return nil, unsupported("isolation verification", "established boundary differs from service policy")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return session.Run(ctx, req)
}

// CheckResume asks the enforcing engine whether the saved session is available
// and compatible. It reads no transcript and launches nothing. An engine without
// this capability selects owned-log replay.
func (e BoundaryExecutor) CheckResume(ctx context.Context, previous, next Profile, session BackendSession) error {
	checker, ok := e.Engine.(ResumeChecker)
	if !ok {
		return unsupported("resume", "isolation engine cannot verify saved session compatibility and availability")
	}
	return checker.CheckResume(ctx, previous, next, session)
}

func cloneIsolation(iso Isolation) Isolation {
	iso.Environment = maps.Clone(iso.Environment)
	iso.Credentials = slices.Clone(iso.Credentials)
	iso.Capabilities.Tools = slices.Clone(iso.Capabilities.Tools)
	return iso
}
func clonePolicy(p BoundaryPolicy) BoundaryPolicy {
	p.Isolation = cloneIsolation(p.Isolation)
	p.Mounts = slices.Clone(p.Mounts)
	return p
}
