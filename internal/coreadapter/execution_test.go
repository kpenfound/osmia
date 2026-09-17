package coreadapter

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/vcs"
)

type fakeExecutor struct {
	checkErr      error
	checks, calls int
	request       agent.Request
	settings      ExecutionSettings
	run           func(context.Context) (*agent.Result, error)
}

func (f *fakeExecutor) Check(context.Context, Isolation, ExecutionSettings) error {
	f.checks++
	return f.checkErr
}
func (f *fakeExecutor) Run(ctx context.Context, r agent.Request, s ExecutionSettings) (*agent.Result, error) {
	f.calls++
	f.request = r
	f.settings = s
	return f.run(ctx)
}
func prepared(t *testing.T) PreparedTurn {
	t.Helper()
	return PreparedTurn{Scope: Scope{Role: "mason", Turn: "turn-1"}, Profile: Profile{Backend: "claude", Model: "chosen", Effort: "high", MaxTurns: 7, Timeout: time.Minute},
		SessionDirectory: t.TempDir(), Prompt: "task", SystemPrompt: "system", History: "history", AllowedOutcomes: []string{"done"},
		Sandbox:   SandboxLease{Verified: Isolation{Workspace: Workspace{Directory: t.TempDir(), Access: ReadWrite}, DenyVCS: true, DenyInheritedEnvironment: true, DenyDeliveryCredentials: true, Environment: map[string]string{"APP": "$LITERAL"}}},
		Execution: ExecutionSettings{Mode: "container", Image: "prepared-image", Mounts: []string{"/approved"}, Domains: []string{"provider.example"}},
		MCP:       []Endpoint{{URL: "http://prepared/mcp", BearerTokenEnvironment: "MCP_TOKEN"}}}
}
func TestTurnTranslationAndProvenance(t *testing.T) {
	t.Setenv("GH_TOKEN", "host-secret")
	turn := prepared(t)
	at := time.Unix(123, 0)
	turn.Resume = &BackendSession{Backend: "claude", ID: "previous"}
	turn.Sandbox.Verified.Capabilities.Tools = []string{"file_read", "file_write"}
	fake := &fakeExecutor{run: func(context.Context) (*agent.Result, error) {
		return &agent.Result{ClaudeID: "session", SessionDir: turn.SessionDirectory, StartedAt: at, Duration: time.Second, CostUSD: 1.25, CostKnown: true, NumTurns: 3, ResultText: "response", HasOutcome: true, Outcome: agent.Outcome{Status: "done", Note: "report"}, RateLimit: &agent.RateLimit{Status: "allowed", Type: "daily", ResetsAt: at}}, nil
	}}
	result, err := (&TurnRunner{Executor: fake}).Run(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	req := fake.request
	if req.Profile.Name != "mason" || req.Name != "turn-1" || req.Profile.Model != "chosen" || req.Profile.Effort != "high" || req.Profile.MaxTurns != 7 || req.Profile.Timeout != time.Minute || req.Profile.Agent != "claude" {
		t.Fatalf("profile: %+v", req)
	}
	if req.Workspace.Directory() != turn.Sandbox.Verified.Workspace.Directory || req.Workspace.VCS() != nil || req.Profile.VCSAccess || len(req.VCSEnv) != 0 || len(req.VCSContainerEnv) != 0 {
		t.Fatalf("VCS resources exposed: %+v", req)
	}
	if !reflect.DeepEqual(req.Env, turn.Sandbox.Verified.Environment) || len(req.Profile.Env) != 0 || len(req.ContainerEnv) != 0 {
		t.Fatalf("environment: %+v", req)
	}
	if req.Prompt != "history\n\ntask" || req.SystemPrompt != "system" || req.ResumeID != "previous" || !reflect.DeepEqual(req.ValidOutcomes, turn.AllowedOutcomes) {
		t.Fatalf("context: %+v", req)
	}
	if req.Profile.MCP["osmia_0"].BearerTokenEnv != "MCP_TOKEN" || len(req.Profile.MCP) != 1 || !reflect.DeepEqual(fake.settings, turn.Execution) || req.Profile.SandboxImage != "prepared-image" {
		t.Fatalf("settings: %+v", req.Profile)
	}
	// Claude matches MCP tools by server-qualified name; bare names pin nothing.
	if !reflect.DeepEqual(req.Profile.AllowedTools, []string{"mcp__osmia_0__file_read", "mcp__osmia_0__file_write"}) {
		t.Fatalf("allowed tools: %v", req.Profile.AllowedTools)
	}
	unhosted := prepared(t)
	unhosted.MCP, unhosted.Sandbox.Verified.Capabilities.Tools = nil, []string{"file_read"}
	if _, err := (&TurnRunner{Executor: fake}).Run(context.Background(), unhosted); !errors.Is(err, ErrUnsupported) || fake.calls != 1 {
		t.Fatalf("granted tools without a service host: %v", err)
	}
	if result.Session.ID != "session" || result.Session.Backend != "claude" || result.Outcome.Report != "report" || result.Usage != (Usage{1.25, true, 3}) || result.StartedAt != at || result.Duration != time.Second || result.FinalResponse != "response" || result.Limit.Kind != "daily" {
		t.Fatalf("result: %+v", result)
	}
}
func TestUnsupportedBeforeExecution(t *testing.T) {
	tests := map[string]func(*PreparedTurn){
		"backend":         func(r *PreparedTurn) { r.Profile.Backend = "unknown" },
		"sandbox":         func(r *PreparedTurn) { r.Execution.Mode = "unknown" },
		"container image": func(r *PreparedTurn) { r.Execution.Image = "" },
		"resume": func(r *PreparedTurn) {
			r.Profile.Backend = "codex"
			r.Profile.MaxTurns = 0
			r.Resume = &BackendSession{Backend: "codex", ID: "x"}
		},
		"turn limit": func(r *PreparedTurn) { r.Profile.Backend = "codex" },
		"cost":       func(r *PreparedTurn) { r.Profile.CostLimitUSD = 2 },
		"credential": func(r *PreparedTurn) { r.Sandbox.Verified.Credentials = []CredentialRef{{"TOKEN", "secret-ref"}} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			turn := prepared(t)
			mutate(&turn)
			f := &fakeExecutor{}
			_, err := (&TurnRunner{Executor: f}).Run(context.Background(), turn)
			var unsupported *UnsupportedError
			if !errors.As(err, &unsupported) || f.calls != 0 {
				t.Fatalf("err=%v calls=%d", err, f.calls)
			}
		})
	}
	turn := prepared(t)
	_, err := (&TurnRunner{Executor: CoreExecutor{Runner: CoreEngine{}}}).Run(context.Background(), turn)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("turn outside the executor's service isolation accepted: %v", err)
	}
	f := &fakeExecutor{checkErr: unsupported("isolation", "not enforced")}
	_, err = (&TurnRunner{Executor: f}).Run(context.Background(), turn)
	if !errors.Is(err, ErrUnsupported) || f.calls != 0 {
		t.Fatal("boundary failure launched executor")
	}
}
func TestOutcomeAllowlistAndPartialFailure(t *testing.T) {
	for _, allowed := range [][]string{nil, {}, {"waiting"}} {
		t.Run("rejected", func(t *testing.T) {
			turn := prepared(t)
			turn.AllowedOutcomes = allowed
			f := &fakeExecutor{run: func(context.Context) (*agent.Result, error) {
				return &agent.Result{HasOutcome: true, Outcome: agent.Outcome{Status: "done"}, ClaudeID: "partial", Signal: 9, ExitCode: -1}, nil
			}}
			result, err := (&TurnRunner{Executor: f}).Run(context.Background(), turn)
			if err == nil || result.Outcome != nil || result.Session.ID != "partial" || result.Signal != 9 || result.ErrorSubtype != "invalid_outcome" {
				t.Fatalf("%+v %v", result, err)
			}
			if f.request.ValidOutcomes == nil {
				t.Fatal("nil permits every core outcome")
			}
		})
	}
}
func TestCleanupAcrossAttemptPaths(t *testing.T) {
	for _, mode := range []string{"success", "failure", "cancel", "pre-cancel", "unsupported", "retain"} {
		t.Run(mode, func(t *testing.T) {
			turn := prepared(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			released, cleaned := 0, 0
			turn.WorkspaceLease = &WorkspaceLease{Workspace: turn.Sandbox.Verified.Workspace, Lease: &releaseLease{release: func(ctx context.Context) error {
				if ctx.Err() != nil {
					t.Fatal("cancelled cleanup")
				}
				released++
				return nil
			}}}
			turn.Cleanup = []Lease{&releaseLease{release: func(ctx context.Context) error {
				if ctx.Err() != nil {
					t.Fatal("cancelled cleanup")
				}
				cleaned++
				return nil
			}}}
			turn.RetainWorkspace = mode == "retain"
			f := &fakeExecutor{run: func(ctx context.Context) (*agent.Result, error) {
				switch mode {
				case "failure":
					return &agent.Result{ClaudeID: "partial", IsError: true}, errors.New("failure")
				case "cancel":
					cancel()
					return nil, ctx.Err()
				}
				return &agent.Result{}, nil
			}}
			if mode == "pre-cancel" {
				cancel()
			}
			if mode == "unsupported" {
				f.checkErr = unsupported("sandbox", "missing")
			}
			result, err := (&TurnRunner{Executor: f}).Run(ctx, turn)
			if cleaned != 1 || (mode != "retain" && released != 1) || (mode == "retain" && released != 0) {
				t.Fatalf("cleanup=%d release=%d", cleaned, released)
			}
			if mode == "cancel" || mode == "pre-cancel" {
				if !errors.Is(err, context.Canceled) || !result.Cancelled {
					t.Fatalf("%+v %v", result, err)
				}
			}
			if mode == "retain" {
				if err := turn.WorkspaceLease.Lease.Release(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

type fakeProvider struct {
	acquired, released int
	req                vcs.Request
	ws                 vcs.Workspace
	err                error
	cancel             context.CancelFunc
}

func (f *fakeProvider) Acquire(_ context.Context, r vcs.Request) (vcs.Workspace, error) {
	f.acquired++
	f.req = r
	if f.cancel != nil {
		f.cancel()
	}
	return f.ws, f.err
}
func (f *fakeProvider) Release(ctx context.Context, w vcs.Workspace) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if w != f.ws {
		return errors.New("wrong workspace")
	}
	f.released++
	return nil
}
func (f *fakeProvider) Prune(context.Context) error { panic("must not prune") }
func TestWorkspaceAcquisitionAndRelease(t *testing.T) {
	for _, mode := range []string{"success", "error", "cancel", "mismatch", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := t.TempDir()
			f := &fakeProvider{ws: vcs.Directory(dir)}
			req := WorkspaceRequest{Directory: dir, SourceDirectory: "source", BaseRevision: "base", Access: ReadWrite}
			if mode == "error" {
				f.err = errors.New("acquire")
			}
			if mode == "cancel" {
				f.cancel = cancel
			}
			if mode == "mismatch" {
				req.Directory = "elsewhere"
			}
			adapter := WorkspaceAdapter{Select: func(_ context.Context, got WorkspaceRequest) (vcs.Provider, vcs.Request, error) {
				if got != req {
					t.Fatal("lost request")
				}
				if mode == "unsupported" {
					return nil, vcs.Request{}, unsupported("workspace", "provider absent")
				}
				return f, vcs.Request{Name: "service-name", Ref: got.BaseRevision, Branch: "service-branch"}, nil
			}}
			lease, err := adapter.Acquire(ctx, req)
			if mode == "success" {
				if err != nil {
					t.Fatal(err)
				}
				if lease.Workspace.ID != "service-name" || f.req.Branch != "service-branch" {
					t.Fatal("naming lost")
				}
				for range 2 {
					if err := lease.Lease.Release(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}
			if mode == "unsupported" {
				if f.acquired != 0 {
					t.Fatal("unsupported acquired")
				}
			} else if f.released != 1 {
				t.Fatalf("released %d", f.released)
			}
		})
	}
}
func TestCleanupFailureRetriable(t *testing.T) {
	calls := 0
	l := &releaseLease{release: func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("cleanup")
		}
		return nil
	}}
	turn := prepared(t)
	turn.Cleanup = []Lease{l}
	f := &fakeExecutor{run: func(context.Context) (*agent.Result, error) { return &agent.Result{ClaudeID: "kept"}, nil }}
	result, err := (&TurnRunner{Executor: f}).Run(context.Background(), turn)
	if err == nil || result.Session.ID != "kept" || !result.IsError {
		t.Fatalf("lost cleanup error or result: %+v %v", result, err)
	}
	if err = l.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = l.Release(context.Background())
	if calls != 2 {
		t.Fatal(calls)
	}
}
