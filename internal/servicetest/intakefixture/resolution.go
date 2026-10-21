package intakefixture

import (
	"context"
	"errors"
	"fmt"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"strings"
	"testing"
)

func (f *ProductionDesign) ScriptResolution(t *testing.T, edge, draftTool string) {
	t.Helper()
	f.Engine.Mu.Lock()
	f.Engine.Turns["project-spec-draft-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		graph := []map[string]any{}
		if edge != "" {
			graph = append(graph, map[string]any{"from": string(*f.Stream1), "to": string(*f.Stream2), "type": edge})
		}
		_, err := CallTool(ctx, tools, draftTool, map[string]any{
			"behavior":  "The server stores chunks with an acknowledgment; the client resumes from that offset.",
			"contracts": []map[string]any{{"name": "acknowledgment", "text": "Stored chunks are acknowledged with their offset."}},
			"graph":     graph,
			"slices":    []map[string]any{{"workstream": string(*f.Stream1), "contract_criteria": []int{2}}, {"workstream": string(*f.Stream2), "contract_criteria": []int{1}}},
		})
		return QuestionResult(req, "session-project-spec", "Project spec delivered."), err
	}
	if edge == "" {
		for i := 1; i <= 2; i++ {
			f.Engine.Turns[intake.RoundTurnID(1, intake.CommitteeAgent(i), 1)] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				content, err := ReadTool(ctx, tools, "context.md")
				if err != nil {
					return nil, err
				}
				if !strings.Contains(content, "Draft project spec") || !strings.Contains(content, "acknowledgment") {
					return nil, errors.New("committee did not receive the shared draft and contract without merge edges")
				}
				return QuestionResult(req, "session-committee-"+req.Name, "The proposed contract and slice fit the charter."), nil
			}
		}
	}
	f.Engine.Mu.Unlock()
}
func (f *ProductionDesign) DeliverResolutionMembers(t *testing.T, publicationDocument, publicationPath string) []api.DeliveryPublication {
	t.Helper()
	var out []api.DeliveryPublication
	for i, m := range []struct {
		project config.ProjectID
		stream  config.WorkstreamID
	}{{*f.Project, *f.Stream1}, {*f.Project2, *f.Stream2}} {
		repo, err := f.Repository(m.project)
		Must(t, err)
		f.LifecycleAwait(t, "member build recorded before delivery", func() bool {
			transitions, err := trace.Read[trace.Transition](repo, m.stream)
			Must(t, err)
			var state string
			for _, transition := range transitions {
				if transition.Subject == trace.FeatureSubject {
					state = transition.To
				}
			}
			return state == BuildingState
		})
		cfg := f.Config().For(m.project)
		p := api.DeliveryPublication{Status: "opened", Upstream: cfg.Project.Upstream, Fork: cfg.Project.PushRepository(), Base: "main", Branch: intake.FeatureBranch(m.stream), Commit: "feature-tip", PullRequest: 101 + i, URL: fmt.Sprintf("https://example.invalid/pull/%d", 101+i)}
		fixturekit.ResolutionDocument(t, repo, m.stream, publicationDocument, publicationPath, p, fixturekit.DemoStart, fixturekit.OwnerActor)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "delivered-for-resolution", Revision: 1, Project: m.project, Workstream: m.stream, At: f.Clock.Now(), Actor: fixturekit.OwnerActor, Cause: "fixture"}
		_, err = repo.SetFeatureState(context.Background(), h, intake.DeliveredState, "Owner-approved ready PR published")
		Must(t, err)
		out = append(out, p)
	}
	return out
}
