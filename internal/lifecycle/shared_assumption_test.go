package lifecycle_test

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	strings "strings"
	testing "testing"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	questions "github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesignProductionSharedManagerAssumptionsReachMemberDraftsAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait})
	defer f.Stop(t)
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(ctx, f.Stream1, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	const shared = "Reuse the established offset retry policy."
	const client = "Use the client's existing retry backoff."
	const marker = "Record the agreed retry judgments."
	f.Engine.Mu.Lock()
	fallback := f.Engine.Turns["*"]
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.HasPrefix(req.Name, "message_") || !strings.HasSuffix(strings.TrimSpace(req.Prompt), marker) {
			return fallback(ctx, req, verified, tools)
		}
		for _, input := range []map[string]any{
			{"text": shared},
			{"text": client, "workstreams": []string{string(f.Stream2)}},
			{"text": client, "workstreams": []string{string(f.Stream2)}},
		} {
			result, err := fixturekit.CallTool(ctx, tools, questions.LogAssumptionTool, input)
			if err != nil {
				return nil, err
			}
			if !strings.Contains(result, `"recorded":true`) {
				return nil, fmt.Errorf("assumption refused: %s", result)
			}
		}
		return intakefixture.QuestionResult(req, "session-manager", "Retry judgments recorded."), nil
	}
	f.Engine.Mu.Unlock()
	sent, err := f.C.Send(ctx, f.Stream2, marker)
	fixturekit.Must(t, err)
	f.Wait(t, "shared assumption turn completion", func() bool {
		entries, err := f.C.Conversation(ctx, f.Stream2)
		fixturekit.Must(t, err)
		for _, e := range entries.Entries {
			if e.Turn == sent.Turn && e.State == api.TurnDone {
				return true
			}
		}
		return false
	})

	check := func() []string {
		t.Helper()
		bodies := []string{}
		for i, member := range []config.WorkstreamID{f.Stream1, f.Stream2} {
			project := f.Project
			if i == 1 {
				project = f.Project2
			}
			repo, err := f.S.Repository(project)
			fixturekit.Must(t, err)
			_, body, err := journey.DraftDelivery(f.S.About(repo), repo, member, api.FinalReport{Spec: 1})
			fixturekit.Must(t, err)
			if strings.Count(body, "- "+shared) != 1 {
				t.Fatalf("member %s draft lacks the shared judgment exactly once:\n%s", member, body)
			}
			count := strings.Count(body, "- "+client)
			if count != i {
				t.Fatalf("member %s client judgment count=%d, want %d:\n%s", member, count, i, body)
			}
			docs, err := trace.Read[trace.Document](repo, member)
			fixturekit.Must(t, err)
			for _, d := range docs {
				if d.Path == trace.AssumptionsPath {
					t.Fatal("shared assumptions were copied to member history")
				}
			}
			bodies = append(bodies, body)
		}
		return bodies
	}
	before := check()
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	authority, host, _, closeAuthority, err := f.S.Lifecycle().ManagerRepository(ctx, repo, f.Stream1)
	fixturekit.Must(t, err)
	doc, err := trace.Get[trace.Document](authority, host, trace.AssumptionsDocument, 2)
	closeAuthority()
	fixturekit.Must(t, err)
	var log struct {
		Assumptions []trace.Assumption `json:"assumptions"`
	}
	fixturekit.Must(t, json.Unmarshal([]byte(doc.Content), &log))
	entries := log.Assumptions
	if len(entries) != 2 || entries[0].Thread != f.Before.Identity.ThreadID || entries[0].Turn != sent.Turn {
		t.Fatalf("shared judgments lack their original turn identity: %+v", entries)
	}
	f.Stop(t)
	f.Start(t)
	after := check()
	if before[0] != after[0] || before[1] != after[1] {
		t.Fatal("restart changed the drafted assumptions")
	}
	f.CheckContinuation(t)
	inbox, err := f.C.Inbox(ctx)
	fixturekit.Must(t, err)
	if len(inbox.Entries) != 0 {
		t.Fatalf("assumption created owner work: %+v", inbox)
	}
}
