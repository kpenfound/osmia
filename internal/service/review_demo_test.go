package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestM3ExactReviewDemonstration exercises review through the local API and trace.
func TestM3ExactReviewDemonstration(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, masons, chief := newAskingMasonFixture(t, 1, independentPlan, p)
	defer f.stop(t)
	masons.play[masonTurnID("resume")] = reportDone("Initial candidate")
	var reviews atomic.Int32
	var old, revised UnitReviewIdentity
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, masonAgent("resume")+"-revise-") {
			if !strings.Contains(req.Prompt, "Handle retry token") {
				return nil, fmt.Errorf("revision lost the finding: %s", req.Prompt)
			}
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), masonWrote), []byte("package trace\n// retry token handled\n"), 0600); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Join(req.Workspace.Directory(), "docs"), 0700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "docs", "proof.md"), []byte("Retry proof\n"), 0600); err != nil {
				return nil, err
			}
			if err := reportDone("Revised candidate")(ctx, req, tools); err != nil {
				return nil, err
			}
			return &agent.Result{ClaudeID: req.Name, ResultText: "Revised", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			identity, err := reviewIdentityInPrompt(req.Prompt)
			if err != nil {
				return nil, err
			}
			n := reviews.Add(1)
			if n == 1 {
				old = identity
			} else {
				revised = identity
			}
			if n == 2 {
				if err := asks(p, "1")(ctx, req, turn, tools); err != nil {
					return nil, err
				}
				return &agent.Result{ClaudeID: req.Name, ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 1}, nil
			}
			v := UnitVerdict{Decision: "satisfactory", Evidence: reviewEvidence(), Findings: []ReviewFinding{}}
			if n == 1 {
				v.Decision = "material_findings"
				v.Findings = []ReviewFinding{{Criterion: "spec#1", Severity: "material", Evidence: "Retry proof fails", Action: "Handle retry token"}}
			}
			if n == 3 {
				v.ExtraPaths = []PathExplanation{{Path: "docs/proof.md", Explanation: "Records the planned retry proof"}}
			}
			args := map[string]any{"decision": v.Decision, "evidence": v.Evidence, "findings": v.Findings}
			if len(v.ExtraPaths) != 0 {
				args["extra_paths"] = v.ExtraPaths
			}
			body, err := callTool(ctx, tools, verdictTool, args)
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.turn(ctx, req, turn, tools)
	}
	f.engine.mu.Unlock()
	f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, relayedRuling) {
			return fmt.Errorf("reviewer did not receive the ruling")
		}
		v := UnitVerdict{Decision: "satisfactory", Evidence: reviewEvidence(), Findings: []ReviewFinding{}}
		body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": v.Decision, "evidence": v.Evidence, "findings": v.Findings})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return fmt.Errorf("answer verdict %s: %v", body, err)
		}
		return nil
	})
	stream, _ := f.builtAs(t, "exact-review")
	mapping, err := kb.Load(f.repository())
	must(t, err)
	mapping.Entities = append(mapping.Entities, kb.Entity{ID: "proof-docs", Name: "Proof documents", Paths: []string{"docs"}})
	must(t, kb.Store(context.Background(), f.repository(), mapping, f.clock.Now(), reviewerActor, "review-context"))
	deadline := time.Now().Add(demoTimeout)
	for {
		state, err := f.repository().Workflow(stream, trace.UnitSubject("resume"))
		must(t, err)
		if state.Value == UnitWaiting {
			break
		}
		if time.Now().After(deadline) {
			thread, _ := f.repository().Thread(stream, reviewerAgent("resume"))
			f.engine.mu.Lock()
			runs := append([]string(nil), f.engine.runs...)
			f.engine.mu.Unlock()
			t.Fatalf("waiting for reviewer question: state %s, reviews %d, thread %+v, runs %v", state.Value, reviews.Load(), thread.Turns, runs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if reviews.Load() != 2 || old.Candidate.Revision == revised.Candidate.Revision || old.Candidate.BaseRevision != revised.Candidate.BaseRevision {
		t.Fatalf("reviewed candidates: old %+v, revised %+v, turns %d", old, revised, reviews.Load())
	}
	f.stop(t)
	f.start(t)
	f.awaitUnit(t, stream, "resume", UnitWaiting)
	f.rule(t, "1")
	f.awaitUnit(t, stream, "resume", UnitApproved)
	if reviews.Load() != 3 {
		t.Fatalf("review turns: %d", reviews.Load())
	}
	if reason := staleReview(old, revised); !strings.HasPrefix(reason, "stale candidate revision") {
		t.Fatalf("old candidate accepted: %q", reason)
	}
	var results []UnitReviewResult
	docs, err := trace.Read[trace.Document](f.repository(), stream)
	must(t, err)
	for _, d := range docs {
		if d.ID == reviewDocument("resume") {
			var result UnitReviewResult
			if json.Unmarshal([]byte(d.Content), &result) == nil && result.Turn != "" {
				results = append(results, result)
			}
		}
	}
	if len(results) != 3 || results[0].Verdict.Decision != "material_findings" || results[1].Verdict.Decision != "satisfactory" || len(results[1].Verdict.ExtraPaths) != 0 || len(results[2].Verdict.ExtraPaths) != 1 {
		t.Fatalf("review trace: %+v", results)
	}
	if results[2].Identity != revised || results[2].Identity.Candidate.SpecRevision == "" || results[2].Identity.Candidate.PlanRevision == "" {
		t.Fatalf("approved identity: %+v", results[2].Identity)
	}
	var footprintBlocked, approved bool
	for _, move := range allTransitions(t, f.trace, stream) {
		if move.Subject != trace.UnitSubject("resume") {
			continue
		}
		footprintBlocked = footprintBlocked || strings.Contains(move.Reason, "unexplained changed path docs/proof.md")
		approved = approved || move.To == UnitApproved && strings.Contains(move.Reason, revised.Candidate.Revision)
	}
	if !footprintBlocked || !approved || f.question(t, stream, "1").State != trace.QuestionAnswered {
		t.Fatalf("missing footprint rejection, exact approval or ruling: blocked %t, approved %t", footprintBlocked, approved)
	}
	thread := f.thread(t, stream, reviewerAgent("resume"))
	if len(thread.Turns) != 4 || thread.Turns[1].Status() != questions.Waiting {
		t.Fatalf("reviewer requests and responses: %+v", thread.Turns)
	}
	masons.check(t)
	p.check(t)
}
