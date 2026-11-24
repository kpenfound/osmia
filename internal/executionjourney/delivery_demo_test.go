package executionjourney

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/followup"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDeliveryDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := journey.NewMasonFixture(t, 1, fixturekit.ValidPlan)
	defer f.Stop(t)
	factory := runtime.Target{Scope: "factory"}
	masons.Play[execution.MasonTurnID("resume")] = intakefixture.ReportDone("Resume uploads")
	masons.Play[execution.MasonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n// Skip acknowledged chunks.\n"), 0600); err != nil {
			return err
		}
		ok, why, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Skip acknowledged chunks"})
		if err != nil || !ok {
			return fmt.Errorf("dedupe report: %s: %v", why, err)
		}
		return nil
	}
	f.Engine.Mu.Lock()
	chief := f.Engine.Turns["*"]
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		switch {
		case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
			if _, err := reviewtools.IdentityInPrompt(req.Prompt); err != nil {
				return nil, err
			}
			body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("review: %s: %v", body, err)
			}
		case strings.HasPrefix(req.Name, "final-1-"):
			upstream, err := intakefixture.ReadTool(ctx, tools, "branch/UPSTREAM.md")
			if err != nil || upstream != "upstream moved\n" {
				return nil, fmt.Errorf("final review did not read rebased upstream: %q: %v", upstream, err)
			}
			body, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Resumable uploads", "findings": []any{
				map[string]any{"severity": "advisory", "criteria": []any{"spec#2"}, "finding": "No test shows acknowledged chunks are skipped after a restart."}}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("final report: %s: %v", body, err)
			}
		default:
			return chief(ctx, req, turn, tools)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.Engine.Mu.Unlock()

	var stream config.WorkstreamID
	fork, hosted := hostRunningService(t, f, func() config.WorkstreamID { return stream })
	stream = builtPaused(f, t, factory, "delivery")[0]

	journey.AdvanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream moved\n"})
	intakefixture.Mutation(t, f.C, "DELETE", "pause", factory)
	f.AwaitMerged(t, stream, "resume")
	f.AwaitMerged(t, stream, "dedupe")
	f.AwaitFeature(t, stream, intake.AssembledState)
	intakefixture.Soon(t, "the first final review to report", func() bool {
		return len(intakefixture.StreamDocuments(t, f.Repository(), stream, delivery.FinalReportDocument)) >= 1
	})
	if added, err := followup.Read(f.Repository(), stream); err != nil || len(added) != 0 {
		t.Fatalf("an advisory finding built follow-ups %+v: %v", added, err)
	}
	masons.Check(t)
	presented, err := f.C.Delivery(ctx, stream)
	fixturekit.Must(t, err)

	if presented.Report.Review != 1 || presented.Report.Criteria[1].Evidence == "" || len(presented.Report.Findings) != 1 || presented.Title == "" || strings.Contains(presented.Draft, "after a restart") {
		t.Fatalf("presentation %+v", presented)
	}
	f.Stop(t)
	repository, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repository.Close()
	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	f.S.Cfg.Project.Landing = "commit-per-unit"
	p := &journey.PublicationFixture{ShedFixture: f, Stream: stream, Repository: repository, Report: presented.Report, Fork: fork, Pulls: hosted}
	f.S.Options.PullRequests = p.Pulls
	edited := presented.Draft + "\nOwner's delivery note.\n"
	approval := p.Approve(t, &edited)
	if approval.Commit != presented.Report.Commit || approval.Description != edited {
		t.Fatalf("approval %+v", approval)
	}
	op := p.Request(t)
	result, err := p.Publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	if len(p.Pulls.Prs) != 1 || p.Pulls.Creates != 1 || p.Pulls.Updates != 1 || p.Pulls.Readies != 1 || p.Pulls.Prs[0].Draft || p.Feature(t) != intake.DeliveredState {
		t.Fatalf("duplicate delivery: %+v", p.Pulls)
	}
	tip, _ := p.ForkBranch(t)
	g := intake.Workspaces(f.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit)
	reviewed, err := g.Commit(ctx, approval.Commit)
	fixturekit.Must(t, err)
	delivered, err := g.Commit(ctx, tip)
	fixturekit.Must(t, err)
	if delivered.Tree != reviewed.Tree || p.Pulls.Prs[0].Body != edited {
		t.Fatalf("wrong delivered tree or description: %s, %+v", tip, p.Pulls.Prs[0])
	}
	if tip == approval.Commit {
		t.Fatalf("commit-per-unit tip %s, reviewed %s", tip, approval.Commit)
	}
	records, err := delivery.Publications(p.Repository, stream)
	fixturekit.Must(t, err)
	if len(records) != 2 || records[1].Push != 1 || records[1].Status != delivery.PublicationOpened || records[1].Reviewed != approval.Commit || records[1].Commit != tip || records[1].PullRequest != p.Pulls.Prs[0].Number {
		t.Fatalf("publication provenance %+v", records)
	}
	if len(intakefixture.StreamDocuments(t, p.Repository, stream, delivery.FinalReportDocument)) != 1 || len(intakefixture.StreamDocuments(t, p.Repository, stream, delivery.DeliveryDocument)) != 1 {
		t.Fatal("review or owner ruling missing from trace")
	}
}
