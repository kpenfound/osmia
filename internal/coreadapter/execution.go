package coreadapter

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/vcs"
)

// UnsupportedError identifies a capability that cannot be supplied before launch.
type UnsupportedError struct{ Capability, Reason string }

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrUnsupported, e.Capability, e.Reason)
}
func (e *UnsupportedError) Unwrap() error         { return ErrUnsupported }
func unsupported(capability, reason string) error { return &UnsupportedError{capability, reason} }

// ExecutionSettings contains only caller-approved mounts and sandbox settings.
// Mounts never include provider VCS resources. The workspace is passed separately.
type ExecutionSettings struct {
	Mode, Image     string
	Domains, Mounts []string
}

// SessionExecutor is the core-facing execution seam. Check must reject boundaries
// it cannot enforce. Run must use Request.Env as the complete environment, without
// host inheritance or expansion, and must not add tools, mounts or credentials.
// Implementations own backend and container cleanup within one attempt.
type SessionExecutor interface {
	Check(context.Context, Isolation, ExecutionSettings) error
	Run(context.Context, agent.Request, ExecutionSettings) (*agent.Result, error)
}

// TurnRunner performs one attempt; it never selects work or advances workflow.
type TurnRunner struct{ Executor SessionExecutor }

var _ Turns = (*TurnRunner)(nil)

func (r *TurnRunner) Run(ctx context.Context, turn PreparedTurn) (result SessionResult, err error) {
	result.Session.Backend = turn.Profile.Backend
	result.SessionDirectory = turn.SessionDirectory
	defer func() {
		if result.Session.ID == "" {
			result.Session = BackendSession{}
		}
		cleanup := context.WithoutCancel(ctx)
		for i := len(turn.Cleanup) - 1; i >= 0; i-- {
			if turn.Cleanup[i] != nil {
				err = errors.Join(err, turn.Cleanup[i].Release(cleanup))
			}
		}
		if turn.WorkspaceLease != nil && !turn.RetainWorkspace && turn.WorkspaceLease.Lease != nil {
			err = errors.Join(err, turn.WorkspaceLease.Lease.Release(cleanup))
		}
		if err != nil {
			result.IsError = true
		}
	}()
	if err = ctx.Err(); err != nil {
		result.Cancelled = errors.Is(err, context.Canceled)
		result.TimedOut = errors.Is(err, context.DeadlineExceeded)
		result.IsError = true
		return
	}
	req, err := translateTurn(turn)
	if err != nil {
		return result, err
	}
	if r.Executor == nil {
		return result, unsupported("executor", "no execution boundary supplied")
	}
	if err = r.Executor.Check(ctx, turn.Sandbox.Verified, turn.Execution); err != nil {
		return result, err
	}
	raw, runErr := r.Executor.Run(ctx, req, turn.Execution)
	if raw != nil {
		result.Session.ID = raw.ClaudeID
		result.SessionDirectory = raw.SessionDir
		result.StartedAt, result.Duration = raw.StartedAt, raw.Duration
		result.FinalResponse = raw.ResultText
		result.ExitCode, result.Signal = raw.ExitCode, raw.Signal
		result.TimedOut, result.IsError, result.ErrorSubtype = raw.TimedOut, raw.IsError, raw.ErrorSubtype
		result.Usage = Usage{raw.CostUSD, raw.CostKnown, raw.NumTurns}
		if raw.RateLimit != nil {
			result.Limit = &ProviderLimit{raw.RateLimit.Status, raw.RateLimit.Type, raw.RateLimit.ResetsAt}
		}
		if raw.HasOutcome {
			if validationErr := agent.ValidateOutcome(turn.Scope.Role, raw.Outcome.Status, req.ValidOutcomes); validationErr != nil {
				runErr = errors.Join(runErr, validationErr)
				result.IsError = true
				result.ErrorSubtype = "invalid_outcome"
			} else {
				result.Outcome = &Outcome{raw.Outcome.Status, raw.Outcome.Note}
			}
		}
	} else if runErr == nil {
		runErr = errors.New("executor returned no result")
	}
	if runErr != nil {
		result.IsError = true
		result.Cancelled = errors.Is(runErr, context.Canceled)
		result.TimedOut = result.TimedOut || errors.Is(runErr, context.DeadlineExceeded)
		if result.ErrorSubtype == "" {
			switch {
			case result.Cancelled:
				result.ErrorSubtype = "cancelled"
			case result.TimedOut:
				result.ErrorSubtype = "timeout"
			default:
				result.ErrorSubtype = "execution"
			}
		}
	}
	return result, runErr
}

func translateTurn(t PreparedTurn) (agent.Request, error) {
	var req agent.Request
	p := t.Profile
	if !slices.Contains([]string{agent.AgentClaude, agent.AgentCodex, agent.AgentOpenCode}, p.Backend) {
		return req, unsupported("backend", p.Backend)
	}
	if p.CostLimitUSD != 0 {
		return req, unsupported("cost limit", "core has no per-session cost cap")
	}
	if p.MaxTurns != 0 && p.Backend != agent.AgentClaude {
		return req, unsupported("turn limit", "backend does not enforce max turns")
	}
	if t.Scope.Role == "" || t.SessionDirectory == "" || t.Sandbox.Verified.Workspace.Directory == "" {
		return req, errors.New("turn requires role, session directory and verified workspace directory")
	}
	if len(t.Sandbox.Verified.Credentials) != 0 {
		return req, unsupported("credential references", "resolve scoped credentials at the execution boundary before preparing a turn")
	}
	if t.WorkspaceLease != nil && t.WorkspaceLease.Workspace != t.Sandbox.Verified.Workspace {
		return req, errors.New("workspace lease does not match verified sandbox workspace")
	}
	req = agent.Request{
		Name: t.Scope.Turn, SessionDir: t.SessionDirectory, SystemPrompt: t.SystemPrompt, Prompt: t.Prompt,
		Workspace: vcs.Directory(t.Sandbox.Verified.Workspace.Directory), Env: maps.Clone(t.Sandbox.Verified.Environment),
		ValidOutcomes: append([]string{}, t.AllowedOutcomes...),
		Profile: agent.Profile{Name: t.Scope.Role, Agent: p.Backend, Model: p.Model, Effort: p.Effort, Timeout: p.Timeout, MaxTurns: p.MaxTurns,
			Sandbox: t.Execution.Mode, SandboxImage: t.Execution.Image, SandboxDomains: slices.Clone(t.Execution.Domains), MCP: map[string]agent.MCPEntry{}, VCSAccess: false},
	}
	var servers []string
	for i := range t.MCP {
		servers = append(servers, fmt.Sprintf("osmia_%d", i))
	}
	if len(t.Sandbox.Verified.Capabilities.Tools) != 0 && len(servers) == 0 {
		return agent.Request{}, unsupported("tools", "granted tools require a service MCP endpoint")
	}
	req.Profile.AllowedTools = AllowedTools(servers, t.Sandbox.Verified.Capabilities.Tools)
	if err := req.Profile.Validate(); err != nil {
		return agent.Request{}, unsupported("sandbox", err.Error())
	}
	if t.Resume != nil {
		if t.Resume.Backend != p.Backend || p.Backend == agent.AgentCodex || !ValidSession(*t.Resume) {
			return agent.Request{}, unsupported("resume", "backend/session cannot resume; caller must render history")
		}
		req.ResumeID = t.Resume.ID
	}
	if t.History != "" {
		req.Prompt = t.History + "\n\n" + t.Prompt
	}
	for i, endpoint := range t.MCP {
		if endpoint.URL == "" {
			return agent.Request{}, errors.New("MCP endpoint URL is empty")
		}
		req.Profile.MCP[servers[i]] = agent.MCPEntry{Type: "http", URL: endpoint.URL, BearerTokenEnv: endpoint.BearerTokenEnvironment}
	}
	return req, nil
}

// AllowedTools names each granted tool the way the backend identifies MCP
// tools, under every service server, so the allow list pins exactly the hosted
// tools. Bare names would match nothing. The pinned core forwards this list to
// Claude only; other backends are bounded by the scoped MCP registry alone.
func AllowedTools(servers, tools []string) []string {
	var allowed []string
	for _, server := range slices.Sorted(slices.Values(servers)) {
		for _, tool := range tools {
			allowed = append(allowed, "mcp__"+server+"__"+tool)
		}
	}
	return allowed
}
