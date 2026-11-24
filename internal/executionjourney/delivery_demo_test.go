package executionjourney

import (
	"context"
	"fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	f, masons := newMasonFixture(t, 1, fixturekit.ValidPlan)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	masons.Play[service.TestMasonTurnID("resume")] = intakefixture.ReportDone("Resume uploads")
	masons.Play[service.TestMasonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n// Skip acknowledged chunks.\n"), 0600); err != nil {
			return err
		}
		ok, why, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Skip acknowledged chunks"})
		if err != nil || !ok {
			return fmt.Errorf("dedupe report: %s: %v", why, err)
		}
		return nil
	}
	f.engine.Mu.Lock()
	chief := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		switch {
		case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
			if _, err := reviewtools.IdentityInPrompt(req.Prompt); err != nil {
				return nil, err
			}
			body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []api.ReviewFinding{}})
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
	f.engine.Mu.Unlock()

	var stream config.WorkstreamID
	fork, hosted := hostRunningService(t, f, func() config.WorkstreamID { return stream })
	stream = f.builtPaused(t, factory, "delivery")[0]

	advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream moved\n"})
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMerged(t, stream, "resume")
	f.awaitMerged(t, stream, "dedupe")
	f.awaitFeature(t, stream, intake.AssembledState)
	intakefixture.Soon(t, "the first final review to report", func() bool {
		return len(intakefixture.StreamDocuments(t, f.repository(), stream, delivery.FinalReportDocument)) >= 1
	})
	if added, err := followup.Read(f.repository(), stream); err != nil || len(added) != 0 {
		t.Fatalf("an advisory finding built follow-ups %+v: %v", added, err)
	}
	masons.Check(t)
	presented, err := f.c.Delivery(ctx, stream)
	fixturekit.Must(t, err)

	if presented.Report.Review != 1 || presented.Report.Criteria[1].Evidence == "" || len(presented.Report.Findings) != 1 || presented.Title == "" || strings.Contains(presented.Draft, "after a restart") {
		t.Fatalf("presentation %+v", presented)
	}
	f.stop(t)
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	defer repository.Close()
	f.s.setSole(&activeProject{repository: repository})
	f.s.cfg.Project.Landing = "commit-per-unit"
	p := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: presented.Report, fork: fork, pulls: hosted}
	f.s.options.PullRequests = p.pulls
	edited := presented.Draft + "\nOwner's delivery note.\n"
	approval := p.approve(t, &edited)
	if approval.Commit != presented.Report.Commit || approval.Description != edited {
		t.Fatalf("approval %+v", approval)
	}
	op := p.request(t)
	result, err := p.publisher().Apply(ctx, op)
	if err != nil || result.Outcome != "succeeded" {
		t.Fatalf("publication %+v: %v", result, err)
	}
	if len(p.pulls.Prs) != 1 || p.pulls.Creates != 1 || p.pulls.Updates != 1 || p.pulls.Readies != 1 || p.pulls.Prs[0].Draft || p.feature(t) != intake.DeliveredState {
		t.Fatalf("duplicate delivery: %+v", p.pulls)
	}
	tip, _ := p.forkBranch(t)
	g := intake.Workspaces(f.s.cfg, intake.BranchesDirectory, config.WorkspacesGit)
	reviewed, err := g.Commit(ctx, approval.Commit)
	fixturekit.Must(t, err)
	delivered, err := g.Commit(ctx, tip)
	fixturekit.Must(t, err)
	if delivered.Tree != reviewed.Tree || p.pulls.Prs[0].Body != edited {
		t.Fatalf("wrong delivered tree or description: %s, %+v", tip, p.pulls.Prs[0])
	}
	if tip == approval.Commit {
		t.Fatalf("commit-per-unit tip %s, reviewed %s", tip, approval.Commit)
	}
	records, err := delivery.Publications(p.repository, stream)
	fixturekit.Must(t, err)
	if len(records) != 2 || records[1].Push != 1 || records[1].Status != delivery.PublicationOpened || records[1].Reviewed != approval.Commit || records[1].Commit != tip || records[1].PullRequest != p.pulls.Prs[0].Number {
		t.Fatalf("publication provenance %+v", records)
	}
	if len(intakefixture.StreamDocuments(t, p.repository, stream, delivery.FinalReportDocument)) != 1 || len(intakefixture.StreamDocuments(t, p.repository, stream, delivery.DeliveryDocument)) != 1 {
		t.Fatal("review or owner ruling missing from trace")
	}
}
