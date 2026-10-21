package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestDeliveryDemonstration follows one feature from ordinary unit turns
// through a final review with an advisory finding, which builds nothing, to
// the owner's decision and publication.
func TestDeliveryDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := newMasonFixture(t, 1, validPlan)
	defer f.stop(t)
	factory := runtime.Target{Scope: "factory"}
	masons.play[masonTurnID("resume")] = reportDone("Resume uploads")
	masons.play[masonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n// Skip acknowledged chunks.\n"), 0600); err != nil {
			return err
		}
		ok, why, err := done(ctx, tools, map[string]any{"outcome": "Skip acknowledged chunks"})
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
			if _, err := reviewIdentityInPrompt(req.Prompt); err != nil {
				return nil, err
			}
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("review: %s: %v", body, err)
			}
		case strings.HasPrefix(req.Name, "final-1-"):
			upstream, err := readTool(ctx, tools, "branch/UPSTREAM.md")
			if err != nil || upstream != "upstream moved\n" {
				return nil, fmt.Errorf("final review did not read rebased upstream: %q: %v", upstream, err)
			}
			body, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Resumable uploads", "findings": []any{
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

	stream := f.builtPaused(t, factory, "delivery")[0]
	// Move upstream after the seal. The final reviewer must inspect the
	// rebased assembly, not the original feature tip.
	advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream moved\n"})
	mutation(t, f.c, "DELETE", "pause", factory)
	f.awaitMerged(t, stream, "resume")
	f.awaitMerged(t, stream, "dedupe")
	f.awaitFeature(t, stream, AssembledState)
	deadline := time.Now().Add(demoTimeout)
	for len(streamDocuments(t, f.repository(), stream, finalReportDocument)) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("first final review did not report")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if added, err := followup.Read(f.repository(), stream); err != nil || len(added) != 0 {
		t.Fatalf("an advisory finding built follow-ups %+v: %v", added, err)
	}
	masons.check(t)
	presented, err := f.c.Delivery(ctx, stream)
	must(t, err)
	// The draft is a short spec summary, not an account of the work: it does
	// not carry the review's findings (spec#3).
	if presented.Report.Review != 1 || presented.Report.Criteria[1].Evidence == "" || len(presented.Report.Findings) != 1 || presented.Title == "" || strings.Contains(presented.Draft, "after a restart") {
		t.Fatalf("presentation %+v", presented)
	}
	f.stop(t)
	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	defer repository.Close()
	f.s.setSole(&activeProject{repository: repository})
	f.s.mu.Lock()
	f.s.cfg.Project.Landing = "commit-per-unit"
	f.s.mu.Unlock()
	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	demoGit(t, home, "init", "--quiet", "--bare", fork)
	demoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	p := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: presented.Report, fork: fork}
	p.pulls = &fakePulls{Fork: func() string { commit, _ := p.forkBranch(t); return commit }}
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
	if len(p.pulls.Prs) != 1 || p.pulls.Creates != 1 || p.feature(t) != DeliveredState {
		t.Fatalf("duplicate delivery: %+v", p.pulls)
	}
	tip, _ := p.forkBranch(t)
	g := workspaces(f.s.cfg, branchesDirectory, config.WorkspacesGit)
	reviewed, err := g.Commit(ctx, approval.Commit)
	must(t, err)
	delivered, err := g.Commit(ctx, tip)
	must(t, err)
	if delivered.Tree != reviewed.Tree || p.pulls.Prs[0].Body != edited {
		t.Fatalf("wrong delivered tree or description: %s, %+v", tip, p.pulls.Prs[0])
	}
	if tip == approval.Commit {
		t.Fatalf("commit-per-unit tip %s, reviewed %s", tip, approval.Commit)
	}
	records, err := publications(p.repository, stream)
	must(t, err)
	if len(records) != 2 || records[1].Approval != 1 || records[1].Reviewed != approval.Commit || records[1].Commit != tip || records[1].DescriptionHash != approval.DescriptionHash || records[1].PullRequest != p.pulls.Prs[0].Number {
		t.Fatalf("publication provenance %+v", records)
	}
	if len(streamDocuments(t, p.repository, stream, finalReportDocument)) != 1 || len(streamDocuments(t, p.repository, stream, deliveryDocument)) != 1 {
		t.Fatal("review or owner ruling missing from trace")
	}
}
