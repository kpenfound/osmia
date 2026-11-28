package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesignProductionSharedManagerActsOnAMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait})
	defer f.Stop(t)
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(ctx, f.Stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	repo, err := f.S.Repository(f.Project2)
	fixturekit.Must(t, err)
	f.Wait(t, "member building", func() bool {
		feature, err := repo.Workflow(f.Stream2, trace.FeatureSubject)
		fixturekit.Must(t, err)
		return feature.Value == intake.BuildingState
	})

	member := string(f.Stream2)
	const marker = "Settle the client's discovered work."
	type call struct {
		tool  string
		input map[string]any
	}
	calls := []call{
		{"factory_context", map[string]any{"workstream": member}},
		{"factory_context", map[string]any{"workstream": "w_00000000000000000000000000000099"}},
		{"record_discovery", map[string]any{"workstream": member, "key": "client-fake", "description": "Update the client's retry fake", "criterion": "spec#1"}},
		{"move_workstream", map[string]any{"workstream": member, "to": "assembled", "note": "Run the client's final review."}},
		{"inspect_code", map[string]any{"workstream": member, "path": "internal/trace/git.go"}},
	}
	results := make([]string, len(calls))
	f.Engine.Mu.Lock()
	fallback := f.Engine.Turns["*"]
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.HasPrefix(req.Name, "message_") || !strings.HasSuffix(strings.TrimSpace(req.Prompt), marker) {
			return fallback(ctx, req, verified, tools)
		}
		for i, c := range calls {
			result, err := fixturekit.CallTool(ctx, tools, c.tool, c.input)
			if err != nil {
				result = fmt.Sprintf("%s failed: %v", c.tool, err)
			}
			results[i] = result
		}
		return intakefixture.QuestionResult(req, "session-chief", "Client work settled."), nil
	}
	f.Engine.Mu.Unlock()
	sent, err := f.C.Send(ctx, f.Stream1, marker)
	fixturekit.Must(t, err)
	f.Wait(t, "shared chief member turn completion", func() bool {
		entries, err := f.C.Conversation(ctx, f.Stream1)
		fixturekit.Must(t, err)
		for _, e := range entries.Entries {
			if e.Turn == sent.Turn && e.State == api.TurnDone {
				return true
			}
		}
		return false
	})

	docs, err := trace.Read[trace.Document](repo, f.Stream2)
	fixturekit.Must(t, err)
	var listed map[string]int
	_ = json.Unmarshal([]byte(results[0]), &listed)
	var units []string
	discovered := 0
	for _, d := range docs {
		if d.Path == plan.PlanPath {
			graph, err := plan.Parse([]byte(d.Content))
			fixturekit.Must(t, err)
			units = units[:0]
			for _, u := range graph.Units {
				units = append(units, u.ID)
			}
		}
		if strings.HasPrefix(d.Path, "discoveries/") {
			discovered++
		}
	}
	if listed[plan.PlanPath] == 0 {
		t.Fatalf("factory_context did not list the member's documents: %s", results[0])
	}
	if !strings.Contains(results[1], "does not share this manager") {
		t.Fatalf("a workstream outside the workgroup was read: %s", results[1])
	}
	if !strings.Contains(results[2], `"recorded":true`) || discovered != 1 {
		t.Fatalf("discovery not recorded on the member (%d): %s", discovered, results[2])
	}
	if len(units) == 0 || !strings.Contains(results[3], "units "+strings.Join(units, ", ")+" have not merged") {
		t.Fatalf("move_workstream did not read the member's sealed plan %v: %s", units, results[3])
	}
	clone := f.Repos[1].Clone
	tip := gitOutput(t, clone, "rev-parse", "--verify", "--quiet", "refs/heads/"+intake.FeatureBranch(f.Stream2))
	if tip == "" {
		tip = gitOutput(t, clone, "rev-parse", "HEAD")
	}
	var inspected struct {
		Commit  string `json:"commit"`
		Content string `json:"content"`
	}
	_ = json.Unmarshal([]byte(results[4]), &inspected)
	if inspected.Commit != tip || inspected.Content != "package trace\n" {
		t.Fatalf("inspect_code did not read the member's clone at %s: %s", tip, results[4])
	}

	authority, host, _, closeAuthority, err := f.S.Lifecycle().ManagerRepository(ctx, repo, f.Stream2)
	fixturekit.Must(t, err)
	hosted, err := trace.Read[trace.Document](authority, host)
	closeAuthority()
	fixturekit.Must(t, err)
	for _, d := range hosted {
		if strings.HasPrefix(d.Path, "discoveries/") {
			t.Fatalf("the member's discovery was recorded on the host: %s", d.Path)
		}
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
