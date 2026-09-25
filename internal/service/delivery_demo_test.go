package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestM3DeliveryDemonstration follows one feature from ordinary unit turns
// through final review, a follow-up, the owner's decision and publication.
func TestM3DeliveryDemonstration(t *testing.T) {
	for _, style := range []string{"commit-per-unit", "squash"} {
		t.Run(style, func(t *testing.T) {
			ctx := context.Background()
			f, masons := newMasonFixture(t, 1, validPlan)
			defer f.stop(t)
			factory := runtime.Target{Scope: "factory"}
			masons.play[masonTurnID("resume")] = reportDone("Resume uploads")
			masons.play[masonTurnID("dedupe")] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
				if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n// Skip acknowledged chunks.\n"), 0600); err != nil {
					return err
				}
				ok, why, err := done(ctx, tools, map[string]any{"outcome": "Skip acknowledged chunks", "criteria": []any{criterionArgs(dedupeReport)}})
				if err != nil || !ok {
					return fmt.Errorf("dedupe report: %s: %v", why, err)
				}
				return nil
			}
			f.engine.mu.Lock()
			chief := f.engine.turns["*"]
			f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				switch {
				case strings.HasPrefix(req.Name, "mason-final-") && strings.HasSuffix(req.Name, "-implement"):
					if !strings.Contains(req.Prompt, "acknowledged chunks") {
						return nil, fmt.Errorf("follow-up lost its gap: %s", req.Prompt)
					}
					if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/proof_test.go"), []byte("package trace\n// Acknowledged chunks are skipped.\n"), 0600); err != nil {
						return nil, err
					}
					ok, why, err := done(ctx, tools, map[string]any{"outcome": "Prove skipped chunks", "criteria": []any{criterionArgs(CriterionReport{Criterion: "spec#2", Done: "prove skipping", Evidence: "proof_test.go", Proof: "reviewer judgement"})}})
					if err != nil || !ok {
						return nil, fmt.Errorf("follow-up report: %s: %v", why, err)
					}
				case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
					criterion := "spec#2"
					if strings.HasPrefix(req.Name, reviewerAgent("resume")) {
						criterion = "spec#1"
					}
					if _, err := reviewIdentityInPrompt(req.Prompt); err != nil {
						return nil, err
					}
					body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "evidence": []ReviewEvidence{{Criterion: criterion, Evidence: "The candidate contains the planned proof"}}, "findings": []ReviewFinding{}})
					if err != nil || !strings.Contains(body, `"recorded":true`) {
						return nil, fmt.Errorf("review: %s: %v", body, err)
					}
				case strings.HasPrefix(req.Name, "final-1-") || strings.HasPrefix(req.Name, "final-2-"):
					upstream, err := readTool(ctx, tools, "branch/UPSTREAM.md")
					if err != nil || upstream != "upstream moved\n" {
						return nil, fmt.Errorf("final review did not read rebased upstream: %q: %v", upstream, err)
					}
					criteria := []any{map[string]any{"criterion": "spec#1", "evidence": "resume landing and TestResume"}}
					if strings.HasPrefix(req.Name, "final-1-") {
						criteria = append(criteria, map[string]any{"criterion": "spec#2", "gap": "No proof that acknowledged chunks are skipped"})
					} else {
						proof, err := readTool(ctx, tools, "branch/internal/trace/proof_test.go")
						if err != nil || !strings.Contains(proof, "Acknowledged chunks") {
							return nil, fmt.Errorf("second review missed follow-up: %q: %v", proof, err)
						}
						criteria = append(criteria, map[string]any{"criterion": "spec#2", "evidence": "follow-up proof_test.go shows acknowledged chunks are skipped"})
					}
					body, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Resumable uploads", "criteria": criteria})
					if err != nil || !strings.Contains(body, `"recorded":true`) {
						return nil, fmt.Errorf("final report: %s: %v", body, err)
					}
				default:
					return chief(ctx, req, turn, tools)
				}
				return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
			}
			f.engine.mu.Unlock()

			stream := f.builtPaused(t, factory, "delivery-"+style)[0]
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
			added, err := followup.Read(f.repository(), stream)
			must(t, err)
			if len(added) != 1 || added[0].Criterion != "spec#2" || added[0].Gap == "" {
				t.Fatalf("follow-up %+v", added)
			}
			unit := added[0].Unit.ID
			f.awaitMerged(t, stream, unit)
			for len(streamDocuments(t, f.repository(), stream, finalReportDocument)) < 2 {
				if time.Now().After(deadline) {
					t.Fatal("second final review did not report")
				}
				time.Sleep(50 * time.Millisecond)
			}
			masons.check(t)
			presented, err := f.c.Delivery(ctx, stream)
			must(t, err)
			if presented.Report.Review != 2 || presented.Report.Criteria[1].Evidence == "" || !strings.Contains(presented.Draft, unit) {
				t.Fatalf("presentation %+v", presented)
			}
			f.stop(t)
			repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			defer repository.Close()
			f.s.mu.Lock()
			f.s.active = &activeProject{repository: repository}
			f.s.cfg.Project.Landing = style
			f.s.mu.Unlock()
			home := filepath.Dir(f.clone)
			fork := filepath.Join(home, "remotes", "owner", "dagger.git")
			must(t, os.MkdirAll(filepath.Dir(fork), 0700))
			demoGit(t, home, "init", "--quiet", "--bare", fork)
			demoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
			p := &publicationFixture{shedFixture: f, stream: stream, repository: repository, report: presented.Report, fork: fork}
			p.pulls = &fakePulls{fork: func() string { commit, _ := p.forkBranch(t); return commit }}
			f.s.options.PullRequests = p.pulls
			edited := presented.Draft + "\nOwner's delivery note.\n"
			approval := p.approve(t, &edited)
			if approval.Commit != presented.Report.Commit || approval.Description != edited {
				t.Fatalf("approval %+v", approval)
			}
			if _, reason, err := f.s.deliveryGate(ctx, repository, stream, presented.Draft); err != nil || !strings.Contains(reason, "description differs") {
				t.Fatalf("changed description passed: %q %v", reason, err)
			}
			moveFeature(t, f, stream, map[string]string{"late.go": "package trace\n"})
			if _, reason, err := f.s.deliveryGate(ctx, repository, stream, edited); err != nil || !strings.Contains(reason, "stale") {
				t.Fatalf("changed branch passed: %q %v", reason, err)
			}
			g := featureWorkspaces(f.s.cfg)
			acquired, err := g.Acquire(ctx, vcs.Request{Name: string(stream), Branch: featureBranch(stream)})
			must(t, err)
			current, _, err := g.Branch(ctx, featureBranch(stream))
			must(t, err)
			must(t, g.Move(ctx, acquired.(workspace.Worktree), current, approval.Commit))
			op := p.request(t)
			f.s.boundary = func(step string) error {
				if step == "publish-opened" {
					return errors.New("interrupted after opening")
				}
				return nil
			}
			if _, err := p.publisher().Apply(ctx, op); err == nil {
				t.Fatal("publication was not interrupted")
			}
			p.reopen(t)
			f.s.boundary = nil
			result, err := p.publisher().Apply(ctx, op)
			if err != nil || result.Outcome != "succeeded" {
				t.Fatalf("publication %+v: %v", result, err)
			}
			if len(p.pulls.prs) != 1 || p.pulls.creates != 1 || p.feature(t) != DeliveredState {
				t.Fatalf("duplicate delivery: %+v", p.pulls)
			}
			tip, _ := p.forkBranch(t)
			reviewed, err := g.Commit(ctx, approval.Commit)
			must(t, err)
			delivered, err := g.Commit(ctx, tip)
			must(t, err)
			if delivered.Tree != reviewed.Tree || p.pulls.prs[0].Body != edited {
				t.Fatalf("wrong delivered tree or description: %s, %+v", tip, p.pulls.prs[0])
			}
			if style == "commit-per-unit" && tip != approval.Commit {
				t.Fatalf("commit-per-unit tip %s, reviewed %s", tip, approval.Commit)
			}
			if style == "squash" && (tip == approval.Commit || !slices.Equal(delivered.Parents, []string{presented.Report.Upstream.Commit})) {
				t.Fatalf("squash commit %+v", delivered)
			}
			records, err := publications(p.repository, stream)
			must(t, err)
			if len(records) != 2 || records[1].Approval != 1 || records[1].Reviewed != approval.Commit || records[1].Commit != tip || records[1].DescriptionHash != approval.DescriptionHash || records[1].PullRequest != p.pulls.prs[0].Number {
				t.Fatalf("publication provenance %+v", records)
			}
			if len(streamDocuments(t, p.repository, stream, finalReportDocument)) != 2 || len(streamDocuments(t, p.repository, stream, deliveryDocument)) != 1 {
				t.Fatal("review or owner ruling missing from trace")
			}
		})
	}
}
