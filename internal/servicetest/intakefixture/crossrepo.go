package intakefixture

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	CrossSpec1 = "# Upload service\n\nThe upload service accepts chunks and acknowledges them.\n\n## Acceptance criteria\n\n1. A chunk is acknowledged once stored.\n2. The acknowledgment names the chunk's offset.\n"
	CrossPlan1 = `{"version": 2, "units": [
  {"id": "accept", "task": "Accept and store chunks.", "acceptance": ["TestAccept passes"], "criteria": ["spec#1", "spec#2"], "depends_on": [], "footprint": ["internal.trace"]}
]}
`
	CrossSpec2 = "# Upload client\n\nThe upload client resumes from the last acknowledged offset.\n\n## Acceptance criteria\n\n1. The client resumes from the last acknowledged offset.\n"
	CrossPlan2 = `{"version": 2, "units": [
  {"id": "resume", "task": "Resume from the last acknowledged offset.", "acceptance": ["TestResume passes"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]}
]}
`
)

func ScriptCrossRepoDrafts(t *testing.T, engine *fixturekit.DemoEngine, members func() (config.WorkstreamID, config.WorkstreamID)) {
	t.Helper()
	// verdict calls the tool and requires the verdict it returns.
	verdict := func(ctx context.Context, tools *mcp.ClientSession, name string, args map[string]any, want string) error {
		got, err := CallTool(ctx, tools, name, args)
		if err != nil {
			return err
		}
		var status intake.DraftStatus
		if err := json.Unmarshal([]byte(got), &status); err != nil {
			return err
		}
		if status.Verdict != want {
			return fmt.Errorf("%s returned %s, want verdict %s", name, got, want)
		}
		return nil
	}
	deliver := func(ctx context.Context, tools *mcp.ClientSession, path, content, want string) error {
		return verdict(ctx, tools, intake.DraftTool, map[string]any{"path": path, "content": content}, want)
	}
	engine.Mu.Lock()
	engine.Turns["draft-1-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		stream1, stream2 := members()
		dir := req.Workspace.Directory()
		var spec, plan string
		switch {
		case strings.Contains(dir, string(stream1)):
			spec, plan = CrossSpec1, CrossPlan1
		case strings.Contains(dir, string(stream2)):
			spec, plan = CrossSpec2, CrossPlan2
		default:
			return nil, fmt.Errorf("unexpected draft workspace %s", dir)
		}
		if err := deliver(ctx, tools, "spec.md", spec, intake.DraftInvalid); err != nil {
			return nil, err
		}
		if err := deliver(ctx, tools, "plan.json", plan, intake.DraftValid); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Draft delivered", SessionDir: req.SessionDir, NumTurns: 2}, nil
	}
	engine.Turns["project-spec-draft-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		stream1, stream2 := members()
		args := map[string]any{
			"behavior": "Chunks the server accepts are acknowledged with their offset; the client resumes uploads from the last acknowledged offset.",
			"contracts": []map[string]any{
				{"name": "acknowledgment", "text": "The server's acknowledgment of a stored chunk names its offset; the client reads that offset to resume."},
			},
			"graph": []map[string]any{
				{"from": string(stream1), "to": string(stream2), "type": "contract"},
			},
			"slices": []map[string]any{
				{"workstream": string(stream1), "contract_criteria": []int{2}},
				{"workstream": string(stream2), "contract_criteria": []int{1}},
			},
		}
		// A delivery without its intended behaviour is reported invalid and
		// replaced by the next one.
		behavior := args["behavior"]
		args["behavior"] = " "
		if err := verdict(ctx, tools, "deliver_project_spec", args, intake.DraftInvalid); err != nil {
			return nil, err
		}
		args["behavior"] = behavior
		if err := verdict(ctx, tools, "deliver_project_spec", args, intake.DraftValid); err != nil {
			return nil, err
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Project spec delivered", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	engine.Mu.Unlock()
}
