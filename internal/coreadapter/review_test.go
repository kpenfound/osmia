package coreadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/review"
	"github.com/kpenfound/busybees/core/vcs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type pipelineExecutor struct {
	mu       sync.Mutex
	requests []agent.Request
	fail     string
	err      error
}

func (f *pipelineExecutor) Check(context.Context, Isolation, ExecutionSettings) error { return nil }
func (f *pipelineExecutor) Run(_ context.Context, req agent.Request, _ ExecutionSettings) (*agent.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	phase := filepath.Base(req.Name)
	result := &agent.Result{ClaudeID: req.Name, SessionDir: req.SessionDir, CostKnown: true, CostUSD: 1, NumTurns: 1, ResultText: `{"findings":[{"category":"correctness","severity":"high","file":"a.go","lines":[2,3],"side":"new","title":"Missing guard","body":"A nil pointer reaches this branch.","evidence":"nil input"}]}`}
	if phase == review.DistillerName {
		result.ResultText = `{"summary":"Prepared change","size":"xs"}`
	}
	if phase == f.fail {
		if f.err != nil {
			return result, f.err
		}
		result.ResultText = "malformed"
	}
	return result, nil
}
func reviewRequest(t *testing.T) ReviewRequest {
	t.Helper()
	turn := prepared(t)
	turn.Scope.Role = "committee"
	turn.Sandbox.Verified.Workspace.Access = ReadOnly
	return ReviewRequest{Turn: turn, Subject: "流/opaque:subject", Candidate: Candidate{"candidate", "base", "spec", "plan"}, Diff: "diff --git a/a.go b/a.go\n+exact bytes\n", ArtifactDirectory: filepath.Join(t.TempDir(), "review"), Angles: []string{"docs", "general"}, Context: []ContextItem{{Source: "spec", Content: "first"}, {Source: "kb", Content: "second"}, {Source: "spec", Content: "third"}, {Source: "memory", SkippedReason: "disabled"}}}
}
func TestReviewExactInputsAndEvidence(t *testing.T) {
	req := reviewRequest(t)
	fake := &pipelineExecutor{}
	result, err := (&ReviewAdapter{Turns: &TurnRunner{Executor: fake}}).Review(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(req.Diff))
	if result.Subject != req.Subject || result.Candidate != req.Candidate || result.DiffSHA256 != hex.EncodeToString(sum[:]) || result.Partial || result.Verdict != "" {
		t.Fatalf("identity/policy: %+v", result)
	}
	if len(result.Findings) == 0 || result.Findings[0].Summary != "Missing guard\n\nA nil pointer reaches this branch." || result.Findings[0].StartLine != 2 || result.Findings[0].EndLine != 3 || result.Findings[0].Evidence != "nil input" {
		t.Fatalf("findings: %+v", result.Findings)
	}
	if len(result.Sessions) != 3 || len(result.Artifacts) < 4 {
		t.Fatalf("artifacts/sessions: %+v", result)
	}
	for _, call := range fake.requests {
		if !strings.HasSuffix(call.Prompt, req.Diff) || call.Profile.VCSAccess || call.Workspace.Directory() != req.Turn.Sandbox.Verified.Workspace.Directory {
			t.Fatalf("execution boundary: %+v", call)
		}
	}
	prompt := fake.requests[0].Prompt
	if !(strings.Index(prompt, "first") < strings.Index(prompt, "second") && strings.Index(prompt, "second") < strings.Index(prompt, "third")) || !strings.Contains(prompt, "memory: disabled") {
		t.Fatal("lost bundle order/provenance")
	}
	data, err := os.ReadFile(filepath.Join(req.ArtifactDirectory, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		Reference ReviewReference
		Context   []ContextItem
		Diff      string
		Angles    []string
	}
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	if input.Diff != req.Diff || !reflect.DeepEqual(input.Context, req.Context) || !reflect.DeepEqual(input.Angles, req.Angles) {
		t.Fatalf("input: %+v", input)
	}
	artifact, err := review.ReadArtifact[ReviewReference](req.ArtifactDirectory)
	if err != nil || artifact.Brief.Ref.Subject != req.Subject || artifact.Brief.Ref.Candidate != req.Candidate || artifact.Brief.Ref.DiffSHA256 != result.DiffSHA256 {
		t.Fatalf("artifact reference: %+v %v", artifact, err)
	}
	if len(artifact.Runs) != 2 || artifact.Runs[0].Angle != "general" || artifact.Runs[1].Angle != "docs" {
		t.Fatal("caller angle configuration lost")
	}
	if _, err := os.Stat(filepath.Join(req.Turn.Sandbox.Verified.Workspace.Directory, review.DiffFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("review wrote into checkout")
	}
}

func TestReviewPartialFailureAndCleanup(t *testing.T) {
	for _, mode := range []string{"distiller", "docs", "all", "malformed", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			req := reviewRequest(t)
			fake := &pipelineExecutor{fail: mode, err: errors.New("fake failure")}
			switch mode {
			case "all":
				req.Angles = []string{"docs"}
				fake.fail = "docs"
			case "malformed":
				fake.fail = "docs"
				fake.err = nil
			case "cancel":
				fake.fail = "distiller"
				fake.err = context.Canceled
			}
			releases := 0
			req.Turn.Cleanup = []Lease{&releaseLease{release: func(context.Context) error { releases++; return nil }}}
			out, err := (&ReviewAdapter{Turns: &TurnRunner{Executor: fake}}).Review(context.Background(), req)
			if releases != 1 || !out.Partial || len(out.Sessions) == 0 || len(out.Artifacts) == 0 {
				t.Fatalf("%+v %v releases %d", out, err, releases)
			}
			if (mode == "distiller" || mode == "all") && err == nil {
				t.Fatal("pipeline failure lost")
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(req.ArtifactDirectory, "input.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestReviewRejectsUnsupportedAndExistingDirectory(t *testing.T) {
	for _, mode := range []string{"angle", "write", "resume", "existing", "core"} {
		t.Run(mode, func(t *testing.T) {
			req := reviewRequest(t)
			fake := &pipelineExecutor{}
			var turns Turns = &TurnRunner{Executor: fake}
			switch mode {
			case "angle":
				req.Angles = []string{"custom"}
			case "write":
				req.Turn.Sandbox.Verified.Capabilities.Execute = true
			case "resume":
				req.Turn.Resume = &BackendSession{Backend: "claude", ID: "previous"}
			case "existing":
				if err := os.Mkdir(req.ArtifactDirectory, 0o700); err != nil {
					t.Fatal(err)
				}
			case "core":
				turns = &TurnRunner{Executor: CoreExecutor{}}
			}
			_, err := (&ReviewAdapter{Turns: turns}).Review(context.Background(), req)
			if err == nil || len(fake.requests) != 0 {
				t.Fatalf("%v calls %d", err, len(fake.requests))
			}
			if mode != "existing" && !errors.Is(err, ErrUnsupported) {
				t.Fatal(err)
			}
		})
	}
}

func TestCompleteAdapterIntegration(t *testing.T) {
	ctx := context.Background()
	req := reviewRequest(t)
	scope := req.Turn.Scope
	capacity, err := NewCapacity(map[string]int{"committee": 3})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := capacity.TryClaim(ctx, ClaimRequest{Scope: scope, Slots: map[string]int{"committee": 3}})
	if err != nil || !claim.Acquired {
		t.Fatalf("%+v %v", claim, err)
	}
	provider := &fakeProvider{ws: vcs.Directory(req.Turn.Sandbox.Verified.Workspace.Directory)}
	workspaces := WorkspaceAdapter{Select: func(context.Context, WorkspaceRequest) (vcs.Provider, vcs.Request, error) {
		return provider, vcs.Request{Name: "files"}, nil
	}}
	workspace, err := workspaces.Acquire(ctx, WorkspaceRequest{Scope: scope, Access: ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	req.Turn.Sandbox.Verified.Workspace = workspace.Workspace
	req.Turn.WorkspaceLease = &workspace
	transport := &memoryTransport{}
	hosted, err := (&MCPHost{Transport: transport}).Host(ctx, HostRequest{Scope: scope, Capabilities: Capabilities{Tools: []string{"notes_read"}}, Tools: []Tool{{Name: "notes_read", InputSchema: json.RawMessage(`{"type":"object"}`), Handle: func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"notes":"context"}`), nil
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	called, err := transport.client.CallTool(ctx, &mcp.CallToolParams{Name: "notes_read", Arguments: map[string]any{}})
	if err != nil || called.IsError {
		t.Fatalf("MCP: %+v %v", called, err)
	}
	req.Turn.MCP = []Endpoint{hosted.Endpoint}
	req.Turn.Cleanup = []Lease{claim.Lease, hosted.Lease}
	result, err := (&ReviewAdapter{Turns: &TurnRunner{Executor: &pipelineExecutor{}}}).Review(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if provider.acquired != 1 || provider.released != 1 {
		t.Fatal("workspace lifetime")
	}
	now := time.Unix(456, 0)
	ledger := NewLedger(t.TempDir(), func() time.Time { return now })
	for _, session := range result.Sessions {
		decision, err := (RetryAdapter{}).Decide(ctx, RetryRequest{Result: session, Attempt: 1, MaxRetries: 1})
		if err != nil || decision.Retry {
			t.Fatalf("%+v %v", decision, err)
		}
		if err := ledger.Append(ctx, LedgerEntry{Scope: scope, AttemptID: session.Session.ID, Usage: session.Usage}); err != nil {
			t.Fatal(err)
		}
	}
	spent, err := ledger.Spend(ctx, SpendQuery{Since: now})
	if err != nil || spent != (Spend{3, 0}) {
		t.Fatalf("%+v %v", spent, err)
	}
	budget, err := (BudgetAdapter{}).Evaluate(ctx, BudgetRequest{Spend: spent, LimitUSD: 3, ResumePercent: 70})
	if err != nil || !budget.Crossed {
		t.Fatalf("%+v %v", budget, err)
	}
	wake := NewWakeups()
	if err := wake.Notify(ctx); err != nil {
		t.Fatal(err)
	}
	if err := wake.Wait(ctx, nil); err != nil {
		t.Fatal(err)
	}
	reconciled, err := ledger.Spend(ctx, SpendQuery{})
	if err != nil || reconciled != spent {
		t.Fatal("wake did not reconcile persisted accounting")
	}
	next, err := capacity.TryClaim(ctx, ClaimRequest{Slots: map[string]int{"committee": 3}})
	if err != nil || !next.Acquired {
		t.Fatal("capacity not released after pipeline")
	}
	if err := next.Lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
