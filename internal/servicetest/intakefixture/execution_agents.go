package intakefixture

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func ScriptLanding(engine *fixturekit.DemoEngine, masons *FakeMasons, turnID func(string) string, chief func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) (*agent.Result, error)) {
	masons.Play[turnID("resume")] = ReportDone("Built")
	engine.Mu.Lock()
	defer engine.Mu.Unlock()
	engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			body, err := CallTool(ctx, tools, "verdict", map[string]any{"decision": "satisfactory", "summary": "The candidate does the task and its acceptance holds", "findings": []api.ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief(ctx, req, verified, tools)
	}
}
func ReportProblem(mu *sync.Mutex, list *[]string, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	*list = append(*list, fmt.Sprintf(format, args...))
}
func CheckProblems(t *testing.T, mu *sync.Mutex, list *[]string) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if len(*list) != 0 {
		t.Fatal(strings.Join(*list, "\n"))
	}
}
